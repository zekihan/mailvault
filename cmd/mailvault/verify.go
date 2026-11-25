package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/builtin"
	"github.com/zekihan/mailvault/internal/state"
)

type verificationReport struct {
	Target       string `json:"target"`
	Scanned      int    `json:"scanned"`
	Verified     int    `json:"verified"`
	Corrupt      int    `json:"corrupt"`
	Missing      int    `json:"missing"`
	Unverifiable int    `json:"unverifiable"`
}

func findVerifyTarget(cfg *config.Config, name string) *config.TargetConfig {
	for i := range cfg.Targets {
		if cfg.Targets[i].Name == name {
			return &cfg.Targets[i]
		}
	}
	return nil
}

func openVerifyReader(ctx context.Context, target plugins.Target, tc *config.TargetConfig) (plugins.TargetBackupReader, error) {
	if named, ok := target.(interface{ SetName(name string) }); ok {
		named.SetName(tc.Name)
	}
	reader, ok := target.(plugins.TargetBackupReader)
	if !ok {
		return nil, fmt.Errorf("target %q does not support backup verification", tc.Name)
	}
	if initializer, ok := target.(plugins.TargetBackupReaderInitializer); ok {
		if err := initializer.InitializeForRead(ctx); err != nil {
			return nil, fmt.Errorf("initialize read access: %w", err)
		}
	}
	return reader, nil
}

func printVerifyReport(cmd *cobra.Command, report verificationReport) error {
	if jsonOutput {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %d scanned, %d verified, %d corrupt, %d missing, %d unverifiable\n", report.Target, report.Scanned, report.Verified, report.Corrupt, report.Missing, report.Unverifiable); err != nil {
			return err
		}
	}
	if report.Corrupt > 0 || report.Missing > 0 || report.Unverifiable > 0 {
		return errors.New("backup verification found unresolved entries")
	}
	return nil
}

func runVerify(cmd *cobra.Command, targetName string) error {
	cfg, err := config.LoadSelected(cfgFile, "", targetName)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	tc := findVerifyTarget(cfg, targetName)
	if tc == nil {
		return fmt.Errorf("unknown target %q", targetName)
	}
	target, err := builtin.Registry().CreateTarget(tc.Type, config.TargetPluginConfig(*tc, cfg.MaxMessageBytes))
	if err != nil {
		return fmt.Errorf("create target: %w", err)
	}
	reader, err := openVerifyReader(cmd.Context(), target, tc)
	if err != nil {
		return err
	}
	defer func() { _ = target.Close() }()
	report, err := verifyTargetWithStateTarget(cmd.Context(), tc.Name, plugins.TargetStateKey(target), reader)
	if err != nil {
		return err
	}
	return printVerifyReport(cmd, report)
}

func newVerifyCommand() *cobra.Command {
	return &cobra.Command{Use: "verify [target]", Short: "Verify stored backup messages against their recorded SHA-256 hashes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return runVerify(cmd, args[0])
	}}
}

func verifyTarget(ctx context.Context, targetName string, reader plugins.TargetBackupReader) (verificationReport, error) {
	return verifyTargetWithStateTarget(ctx, targetName, targetName, reader)
}

type verifyPhysicalScan struct {
	locators            []string
	byHash              map[string]string
	hashes              map[string]string
	errorsByLocator     map[string]error
	locatorsByCanonical map[string]string
	counts              map[string]int
	unpadded            map[string]string
	matcher             interface {
		BackupLocatorsEquivalent(first, second string) bool
	}
	matcherOK bool
}

func newVerifyPhysicalScan(reader plugins.TargetBackupReader) *verifyPhysicalScan {
	matcher, matchStructure := reader.(interface {
		BackupLocatorsEquivalent(first, second string) bool
	})
	return &verifyPhysicalScan{
		byHash:              make(map[string]string),
		hashes:              make(map[string]string),
		errorsByLocator:     make(map[string]error),
		locatorsByCanonical: make(map[string]string),
		counts:              make(map[string]int),
		unpadded:            make(map[string]string),
		matcher:             matcher,
		matcherOK:           matchStructure,
	}
}

func (scan *verifyPhysicalScan) indexEntry(ctx context.Context, reader plugins.TargetBackupReader, report *verificationReport, entry *plugins.BackupMessage) error {
	canonical := canonicalBackupLocator(reader, entry.Locator)
	scan.locators = append(scan.locators, canonical)
	report.Scanned++
	scan.counts[canonical]++
	if scan.counts[canonical] > 1 {
		scan.errorsByLocator[canonical] = plugins.ErrBackupMessageCorrupt
		delete(scan.hashes, canonical)
		return nil
	}
	scan.locatorsByCanonical[canonical] = entry.Locator
	msg, readErr := reader.ReadBackupMessage(ctx, entry.Locator)
	if readErr != nil {
		if errors.Is(readErr, plugins.ErrBackupMessageMissing) || errors.Is(readErr, plugins.ErrBackupMessageCorrupt) {
			scan.errorsByLocator[canonical] = readErr
			return nil
		}
		return fmt.Errorf("read target message %q: %w", entry.Locator, readErr)
	}
	scan.hashes[canonical] = dedup.ComputeContentHash(msg.Raw)
	if scan.matcherOK && len(msg.Raw) > 0 && msg.Raw[len(msg.Raw)-1] == '\n' {
		scan.unpadded[canonical] = dedup.ComputeContentHash(msg.Raw[:len(msg.Raw)-1])
	}
	return nil
}

func enumerateVerifyPhysical(ctx context.Context, reader plugins.TargetBackupReader, report *verificationReport, scan *verifyPhysicalScan) error {
	return plugins.WalkBackupMessages(ctx, reader, func(entry plugins.BackupMessage) error {
		return scan.indexEntry(ctx, reader, report, &entry)
	})
}

func indexVerifyByHash(locators []string, hashes, byHash map[string]string) {
	for _, canonical := range locators {
		hash, ok := hashes[canonical]
		if !ok {
			continue
		}
		if previous := byHash[hash]; previous == "" || canonical < previous {
			byHash[hash] = canonical
		}
	}
}

func classifyVerifyWithoutState(report *verificationReport, locators []string, errsByLocator map[string]error) {
	for _, canonical := range locators {
		err := errsByLocator[canonical]
		if err != nil {
			if errors.Is(err, plugins.ErrBackupMessageCorrupt) {
				report.Corrupt++
			} else if errors.Is(err, plugins.ErrBackupMessageMissing) {
				report.Missing++
			}
		} else {
			report.Unverifiable++
		}
	}
}

func canonicalizeSuperseded(reader plugins.TargetBackupReader, superseded map[string]bool) map[string]bool {
	canonicalSuperseded := make(map[string]bool, len(superseded))
	for locator := range superseded {
		canonicalSuperseded[canonicalBackupLocator(reader, locator)] = true
	}
	return canonicalSuperseded
}

func buildVerifyManifestIndex(reader plugins.TargetBackupReader, manifest []plugins.BackupMessage, matchStructure bool) (map[string]map[string]bool, map[string]string, map[string]bool, map[string]bool) {
	expectedByLocator := make(map[string]map[string]bool)
	originalByLocator := make(map[string]string)
	conflicting := make(map[string]bool)
	known := make(map[string]bool)
	for i := range manifest {
		item := &manifest[i]
		canonical := canonicalBackupLocator(reader, item.Locator)
		known[canonical] = true
		if original, exists := originalByLocator[canonical]; matchStructure && exists && original != item.Locator {
			conflicting[canonical] = true
		}
		originalByLocator[canonical] = item.Locator
		if expectedByLocator[canonical] == nil {
			expectedByLocator[canonical] = make(map[string]bool)
		}
		expectedByLocator[canonical][item.RawSHA256] = true
	}
	return expectedByLocator, originalByLocator, conflicting, known
}

func reconcileVerifyManifest(report *verificationReport, scan *verifyPhysicalScan, expectedByLocator map[string]map[string]bool, originalByLocator map[string]string, conflicting map[string]bool) {
	for locator, expected := range expectedByLocator {
		reconcileVerifyLocator(report, scan, locator, expected, originalByLocator, conflicting)
	}
}

func reconcileVerifyLocator(report *verificationReport, scan *verifyPhysicalScan, locator string, expected map[string]bool, originalByLocator map[string]string, conflicting map[string]bool) {
	if len(expected) != 1 || conflicting[locator] {
		report.Corrupt++
		return
	}
	currentLocator, present := scan.locatorsByCanonical[locator]
	if !present {
		report.Missing++
		return
	}
	if readErr := scan.errorsByLocator[locator]; readErr != nil {
		if errors.Is(readErr, plugins.ErrBackupMessageMissing) {
			report.Missing++
		} else {
			report.Corrupt++
		}
		return
	}
	hash := resolveVerifyHash(report, scan, locator, currentLocator, originalByLocator[locator])
	if hash == "" {
		return
	}
	if expected[hash] {
		report.Verified++
	} else {
		report.Corrupt++
	}
}

func resolveVerifyHash(report *verificationReport, scan *verifyPhysicalScan, canonical, currentLocator, original string) string {
	hash := scan.hashes[canonical]
	if !scan.matcherOK {
		return hash
	}
	if !scan.matcher.BackupLocatorsEquivalent(currentLocator, original) {
		report.Corrupt++
		return ""
	}
	if currentLocator != original {
		unpadded, ok := scan.unpadded[canonical]
		if !ok {
			report.Corrupt++
			return ""
		}
		return unpadded
	}
	return hash
}

func reconcileVerifyLegacy(report *verificationReport, legacy []plugins.BackupMessage, byHash map[string]string, known map[string]bool) {
	legacyVerified := make(map[string]bool)
	for i := range legacy {
		item := &legacy[i]
		if item.RawSHA256 == "" {
			report.Unverifiable++
			continue
		}
		if locator := byHash[item.RawSHA256]; locator != "" {
			if !known[locator] {
				legacyVerified[locator] = true
			}
			known[locator] = true
		} else {
			report.Unverifiable++
		}
	}
	report.Verified += len(legacyVerified)
}

func classifyVerifyUnknownPhysical(report *verificationReport, locators []string, superseded, known map[string]bool, errsByLocator map[string]error) {
	for _, locator := range locators {
		isSuperseded := superseded[locator]
		if known[locator] || isSuperseded {
			continue
		}
		err := errsByLocator[locator]
		switch {
		case err == nil:
			report.Unverifiable++
		case errors.Is(err, plugins.ErrBackupMessageCorrupt):
			report.Corrupt++
		case errors.Is(err, plugins.ErrBackupMessageMissing):
			report.Missing++
		}
	}
}

func verifyTargetWithStateTarget(ctx context.Context, targetName, stateTarget string, reader plugins.TargetBackupReader) (verificationReport, error) {
	report := verificationReport{Target: targetName}
	// Retain bounded reconciliation indexes, while reading each message before
	// advancing enumeration. Mbox hashes always come from current record extents.
	scan := newVerifyPhysicalScan(reader)
	if err := enumerateVerifyPhysical(ctx, reader, &report, scan); err != nil {
		return report, fmt.Errorf("list target messages: %w", err)
	}
	// Build legacy matching only after enumeration: duplicate canonical
	// locations must not leave a previously indexed hash eligible for recovery.
	indexVerifyByHash(scan.locators, scan.hashes, scan.byHash)
	store, err := openReadOnlyState(ctx)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return report, fmt.Errorf("open state database: %w", err)
		}
		classifyVerifyWithoutState(&report, scan.locators, scan.errorsByLocator)
		return report, nil
	}
	manifest, err := store.BackupMessages(ctx, stateTarget)
	if err != nil {
		return report, errors.Join(fmt.Errorf("read backup manifest: %w", err), store.Close())
	}
	superseded, err := store.SupersededBackupObjects(ctx, stateTarget)
	if err != nil {
		return report, errors.Join(fmt.Errorf("read repair history: %w", err), store.Close())
	}
	canonicalSuperseded := canonicalizeSuperseded(reader, superseded)
	expectedByLocator, originalByLocator, conflicting, known := buildVerifyManifestIndex(reader, manifest, scan.matcherOK)
	reconcileVerifyManifest(&report, scan, expectedByLocator, originalByLocator, conflicting)
	legacy, err := store.LegacyBackupCandidates(ctx, stateTarget)
	if err != nil {
		return report, errors.Join(fmt.Errorf("read legacy delivery records: %w", err), store.Close())
	}
	reconcileVerifyLegacy(&report, legacy, scan.byHash, known)
	classifyVerifyUnknownPhysical(&report, scan.locators, canonicalSuperseded, known, scan.errorsByLocator)
	return report, store.Close()
}

func canonicalBackupLocator(reader plugins.TargetBackupReader, locator string) string {
	if c, ok := reader.(interface{ CanonicalBackupLocator(locator string) string }); ok {
		return c.CanonicalBackupLocator(locator)
	}
	if resolver, ok := reader.(interface {
		ResolveBackupLocator(locator string) (string, error)
	}); ok {
		if path, err := resolver.ResolveBackupLocator(locator); err == nil {
			return path
		}
	}
	return locator
}

func openReadOnlyState(ctx context.Context) (*state.Store, error) {
	dir := stateDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".local", "share", "mailvault")
	}
	path := filepath.Join(dir, "state.db")
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return state.NewReadOnlyStore(ctx, path)
}
