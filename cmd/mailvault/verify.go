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

func newVerifyCommand() *cobra.Command {
	return &cobra.Command{Use: "verify [target]", Short: "Verify stored backup messages against their recorded SHA-256 hashes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.LoadSelected(cfgFile, "", args[0])
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		var tc *config.TargetConfig
		for i := range cfg.Targets {
			if cfg.Targets[i].Name == args[0] {
				tc = &cfg.Targets[i]
				break
			}
		}
		if tc == nil {
			return fmt.Errorf("unknown target %q", args[0])
		}
		target, err := builtin.Registry().CreateTarget(tc.Type, config.TargetPluginConfig(*tc, cfg.MaxMessageBytes))
		if err != nil {
			return fmt.Errorf("create target: %w", err)
		}
		if named, ok := target.(interface{ SetName(string) }); ok {
			named.SetName(tc.Name)
		}
		reader, ok := target.(plugins.TargetBackupReader)
		if !ok {
			return fmt.Errorf("target %q does not support backup verification", tc.Name)
		}
		if initializer, ok := target.(plugins.TargetBackupReaderInitializer); ok {
			if err := initializer.InitializeForRead(cmd.Context()); err != nil {
				return fmt.Errorf("initialize read access: %w", err)
			}
		}
		defer func() { _ = target.Close() }()
		report, err := verifyTargetWithStateTarget(cmd.Context(), tc.Name, plugins.TargetStateKey(target), reader)
		if err != nil {
			return err
		}
		if jsonOutput {
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
				return err
			}
			if report.Corrupt > 0 || report.Missing > 0 || report.Unverifiable > 0 {
				return fmt.Errorf("backup verification found unresolved entries")
			}
			return nil
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %d scanned, %d verified, %d corrupt, %d missing, %d unverifiable\n", report.Target, report.Scanned, report.Verified, report.Corrupt, report.Missing, report.Unverifiable)
		if err != nil {
			return err
		}
		if report.Corrupt > 0 || report.Missing > 0 || report.Unverifiable > 0 {
			return fmt.Errorf("backup verification found unresolved entries")
		}
		return nil
	}}
}

func verifyTarget(ctx context.Context, targetName string, reader plugins.TargetBackupReader) (report verificationReport, runErr error) {
	return verifyTargetWithStateTarget(ctx, targetName, targetName, reader)
}

func verifyTargetWithStateTarget(ctx context.Context, targetName, stateTarget string, reader plugins.TargetBackupReader) (report verificationReport, runErr error) {
	report = verificationReport{Target: targetName}
	// Retain bounded reconciliation indexes, while reading each message before
	// advancing enumeration. Mbox hashes always come from current record extents.
	var locators []string
	byHash := make(map[string]string)
	physicalHashes, physicalErrors := make(map[string]string), make(map[string]error)
	physicalLocators, physicalCounts := make(map[string]string), make(map[string]int)
	physicalUnpaddedHashes := make(map[string]string)
	matcher, matchStructure := reader.(interface{ BackupLocatorsEquivalent(string, string) bool })
	err := plugins.WalkBackupMessages(ctx, reader, func(entry plugins.BackupMessage) error {
		canonical := canonicalBackupLocator(reader, entry.Locator)
		locators = append(locators, canonical)
		report.Scanned++
		physicalCounts[canonical]++
		if physicalCounts[canonical] > 1 {
			physicalErrors[canonical] = plugins.ErrBackupMessageCorrupt
			delete(physicalHashes, canonical)
			return nil
		}
		physicalLocators[canonical] = entry.Locator
		msg, readErr := reader.ReadBackupMessage(ctx, entry.Locator)
		if readErr != nil {
			if errors.Is(readErr, plugins.ErrBackupMessageMissing) || errors.Is(readErr, plugins.ErrBackupMessageCorrupt) {
				physicalErrors[canonical] = readErr
				return nil
			}
			return fmt.Errorf("read target message %q: %w", entry.Locator, readErr)
		}
		physicalHashes[canonical] = dedup.ComputeContentHash(msg.Raw)
		if matchStructure && len(msg.Raw) > 0 && msg.Raw[len(msg.Raw)-1] == '\n' {
			physicalUnpaddedHashes[canonical] = dedup.ComputeContentHash(msg.Raw[:len(msg.Raw)-1])
		}
		return nil
	})
	if err != nil {
		return report, fmt.Errorf("list target messages: %w", err)
	}
	// Build legacy matching only after enumeration: duplicate canonical
	// locations must not leave a previously indexed hash eligible for recovery.
	for _, canonical := range locators {
		if hash, ok := physicalHashes[canonical]; ok {
			if previous := byHash[hash]; previous == "" || canonical < previous {
				byHash[hash] = canonical
			}
		}
	}
	store, err := openReadOnlyState()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return report, fmt.Errorf("open state database: %w", err)
		}
		for _, canonical := range locators {
			if physicalErrors[canonical] != nil {
				if errors.Is(physicalErrors[canonical], plugins.ErrBackupMessageCorrupt) {
					report.Corrupt++
				} else if errors.Is(physicalErrors[canonical], plugins.ErrBackupMessageMissing) {
					report.Missing++
				}
			} else {
				report.Unverifiable++
			}
		}
		return report, nil
	}
	defer func() { runErr = errors.Join(runErr, store.Close()) }()
	manifest, err := store.BackupMessages(ctx, stateTarget)
	if err != nil {
		return report, fmt.Errorf("read backup manifest: %w", err)
	}
	superseded, err := store.SupersededBackupObjects(ctx, stateTarget)
	if err != nil {
		return report, fmt.Errorf("read repair history: %w", err)
	}
	canonicalSuperseded := make(map[string]bool, len(superseded))
	for locator := range superseded {
		canonicalSuperseded[canonicalBackupLocator(reader, locator)] = true
	}
	expectedByLocator, originalByLocator := make(map[string]map[string]bool), make(map[string]string)
	conflictingLocators := make(map[string]bool)
	known := make(map[string]bool)
	for _, item := range manifest {
		canonical := canonicalBackupLocator(reader, item.Locator)
		known[canonical] = true
		if original, exists := originalByLocator[canonical]; matchStructure && exists && original != item.Locator {
			conflictingLocators[canonical] = true
		}
		originalByLocator[canonical] = item.Locator
		if expectedByLocator[canonical] == nil {
			expectedByLocator[canonical] = make(map[string]bool)
		}
		expectedByLocator[canonical][item.RawSHA256] = true
	}
	for locator, expected := range expectedByLocator {
		if len(expected) != 1 || conflictingLocators[locator] {
			report.Corrupt++
			continue
		}
		currentLocator, present := physicalLocators[locator]
		if !present {
			report.Missing++
			continue
		}
		if readErr := physicalErrors[locator]; readErr != nil {
			if errors.Is(readErr, plugins.ErrBackupMessageMissing) {
				report.Missing++
			} else {
				report.Corrupt++
			}
			continue
		}
		hash := physicalHashes[locator]
		if matchStructure {
			if !matcher.BackupLocatorsEquivalent(currentLocator, originalByLocator[locator]) {
				report.Corrupt++
				continue
			}
			// Equivalent locators can differ only by the writer-added final
			// newline. Both hashes come from the current physical record;
			// never re-read a historical byte range from the manifest.
			if currentLocator != originalByLocator[locator] {
				var ok bool
				hash, ok = physicalUnpaddedHashes[locator]
				if !ok {
					report.Corrupt++
					continue
				}
			}
		}
		if expected[hash] {
			report.Verified++
		} else {
			report.Corrupt++
		}
	}
	legacy, err := store.LegacyBackupCandidates(ctx, stateTarget)
	if err != nil {
		return report, fmt.Errorf("read legacy delivery records: %w", err)
	}
	legacyVerified := make(map[string]bool)
	for _, item := range legacy {
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
	for locator := range legacyVerified {
		report.Verified++
		_ = locator
	}
	for _, locator := range locators {
		isSuperseded := canonicalSuperseded[locator]
		if !known[locator] && !isSuperseded && physicalErrors[locator] == nil {
			report.Unverifiable++
		}
		if !known[locator] && !isSuperseded && errors.Is(physicalErrors[locator], plugins.ErrBackupMessageCorrupt) {
			report.Corrupt++
		}
		if !known[locator] && !isSuperseded && errors.Is(physicalErrors[locator], plugins.ErrBackupMessageMissing) {
			report.Missing++
		}
	}
	return report, nil
}

func canonicalBackupLocator(reader plugins.TargetBackupReader, locator string) string {
	if c, ok := reader.(interface{ CanonicalBackupLocator(string) string }); ok {
		return c.CanonicalBackupLocator(locator)
	}
	if resolver, ok := reader.(interface{ ResolveBackupLocator(string) (string, error) }); ok {
		if path, err := resolver.ResolveBackupLocator(locator); err == nil {
			return path
		}
	}
	return locator
}

func openReadOnlyState() (*state.Store, error) {
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
	return state.NewReadOnlyStore(path)
}
