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
		target, err := builtin.Registry().CreateTarget(tc.Type, tc.Config)
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
	entries, err := reader.ListBackupMessages(ctx)
	if err != nil {
		return report, fmt.Errorf("list target messages: %w", err)
	}
	report.Scanned = len(entries)
	byHash := make(map[string][]string)
	physicalHashes, physicalErrors := make(map[string]string), make(map[string]error)
	for _, entry := range entries {
		canonical := canonicalBackupLocator(reader, entry.Locator)
		msg, readErr := reader.ReadBackupMessage(ctx, entry.Locator)
		if readErr != nil {
			if errors.Is(readErr, plugins.ErrBackupMessageMissing) || errors.Is(readErr, plugins.ErrBackupMessageCorrupt) {
				physicalErrors[canonical] = readErr
				continue
			}
			return report, fmt.Errorf("read target message %q: %w", entry.Locator, readErr)
		}
		hash := dedup.ComputeContentHash(msg.Raw)
		physicalHashes[canonical] = hash
		byHash[hash] = append(byHash[hash], canonical)
	}
	store, err := openReadOnlyState()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return report, fmt.Errorf("open state database: %w", err)
		}
		for _, entry := range entries {
			canonical := canonicalBackupLocator(reader, entry.Locator)
			if physicalErrors[canonical] != nil {
				if errors.Is(physicalErrors[canonical], plugins.ErrBackupMessageCorrupt) {
					report.Corrupt++
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
	expectedByLocator, originalByLocator := make(map[string]map[string]bool), make(map[string]string)
	known := make(map[string]bool)
	for _, item := range manifest {
		canonical := canonicalBackupLocator(reader, item.Locator)
		known[canonical] = true
		originalByLocator[canonical] = item.Locator
		if expectedByLocator[canonical] == nil {
			expectedByLocator[canonical] = make(map[string]bool)
		}
		expectedByLocator[canonical][item.RawSHA256] = true
	}
	verifiedLocators := make(map[string]bool)
	for locator, expected := range expectedByLocator {
		if len(expected) != 1 {
			report.Corrupt++
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
		msg, readErr := reader.ReadBackupMessage(ctx, originalByLocator[locator])
		if readErr != nil {
			if errors.Is(readErr, plugins.ErrBackupMessageMissing) {
				report.Missing++
			} else if errors.Is(readErr, plugins.ErrBackupMessageCorrupt) {
				report.Corrupt++
			} else {
				return report, fmt.Errorf("verify %q: %w", originalByLocator[locator], readErr)
			}
			continue
		}
		hash := dedup.ComputeContentHash(msg.Raw)
		if expected[hash] {
			report.Verified++
			verifiedLocators[locator] = true
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
		if locators := byHash[item.RawSHA256]; len(locators) > 0 {
			locator := locators[0]
			known[locator] = true
			if !verifiedLocators[locator] {
				legacyVerified[locator] = true
			}
		} else {
			report.Unverifiable++
		}
	}
	for locator := range legacyVerified {
		report.Verified++
		_ = locator
	}
	for _, entry := range entries {
		locator := canonicalBackupLocator(reader, entry.Locator)
		isSuperseded := superseded[entry.Locator] || superseded[locator]
		if !known[locator] && !isSuperseded && physicalErrors[locator] == nil {
			report.Unverifiable++
		}
		if !known[locator] && !isSuperseded && errors.Is(physicalErrors[locator], plugins.ErrBackupMessageCorrupt) {
			report.Corrupt++
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
