package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/builtin"
	"github.com/zekihan/mailvault/internal/state"
	mailvaultsync "github.com/zekihan/mailvault/internal/sync"
	"github.com/zekihan/mailvault/internal/testutil/mailtest"
)

type syncBackupFixture struct {
	targetPath string
	dbPath     string
}

func setupSyncBackupFixture(t *testing.T) syncBackupFixture {
	t.Helper()
	messages := syncBackupMessages()
	_, port := mailtest.POP3Server(t, messages, nil)
	configureCLIOnPort(t, port, "3s")
	jsonOutput = true
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	configuredTarget := cfg.Targets[0]
	backup, err := builtin.Registry().CreateTarget(configuredTarget.Type, config.TargetPluginConfig(configuredTarget, cfg.MaxMessageBytes))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backup.Close() })
	return syncBackupFixture{
		targetPath: mustAssert[string](t, configuredTarget.Config["path"]),
		dbPath:     filepath.Join(stateDir, "state.db"),
	}
}

func syncBackupMessages() []mailtest.POP3Message {
	return []mailtest.POP3Message{
		{UID: "healthy", Raw: "Subject: healthy\r\n\r\nfirst body\r\n"},
		{UID: "corrupt", Raw: "Subject: corrupt\r\n\r\nsecond body\r\n"},
		{UID: "missing", Raw: "Subject: missing\r\n\r\nthird body\r\n"},
	}
}

func assertSyncBackupStats(t *testing.T, output string, err error, fetched, written int) {
	t.Helper()
	var stats mailvaultsync.SyncStats
	decodeErr := json.Unmarshal([]byte(output), &stats)
	if err != nil || decodeErr != nil || stats.TotalFetched != fetched || stats.TotalWritten != written || stats.TotalErrors != 0 {
		t.Fatalf("sync output = %s, error = %v, decode = %v", output, err, decodeErr)
	}
}

func runSyncExpectBackup(t *testing.T, fetched, written int) {
	t.Helper()
	output, err := captureOutput(t, func() error { return runSync(nil, nil) })
	assertSyncBackupStats(t, output, err, fetched, written)
}

func assertDryRunCreatedNothing(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dry run created %s: %v", path, err)
		}
	}
}

func manifestLocators(t *testing.T, fixture syncBackupFixture, backup plugins.Target, messages []mailtest.POP3Message) map[string]string {
	t.Helper()
	store, err := state.NewReadOnlyStore(t.Context(), fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entries, err := store.BackupMessages(t.Context(), plugins.TargetStateKey(backup))
	if err != nil || len(entries) != len(messages) {
		t.Fatalf("manifest entries = %d, error = %v", len(entries), err)
	}
	paths := make(map[string]string)
	for i := range entries {
		paths[entries[i].UID] = filepath.Join(fixture.targetPath, filepath.FromSlash(entries[i].Locator))
	}
	return paths
}

func assertBackupContents(t *testing.T, locators map[string]string, messages []mailtest.POP3Message, stage string) {
	t.Helper()
	for _, msg := range messages {
		if got := readCommandFixture(t, locators[msg.UID]); string(got) != msg.Raw {
			t.Fatalf("%s backup differs from fixture for UID %s", stage, msg.UID)
		}
	}
}

func damageBackupMessages(t *testing.T, original map[string]string) {
	t.Helper()
	const damage = "damaged fixture"
	if err := os.WriteFile(original["corrupt"], []byte(damage), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(original["missing"]); err != nil {
		t.Fatal(err)
	}
}

func assertDamagedCopies(t *testing.T, original map[string]string) {
	t.Helper()
	if got := readCommandFixture(t, original["corrupt"]); string(got) != "damaged fixture" {
		t.Fatal("repair changed the damaged original")
	}
	if _, err := os.Stat(original["missing"]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing locator unexpectedly exists: %v", err)
	}
}

func armRepairCommand(t *testing.T) func(args ...string) (string, error) {
	t.Helper()
	repair, _, err := rootCmd.Find([]string{"repair"})
	if err != nil {
		t.Fatal(err)
	}
	previousSource, previousTarget := repairSource, repairTarget
	previousSilenceErrors, previousSilenceUsage := repair.SilenceErrors, repair.SilenceUsage
	repair.SilenceErrors, repair.SilenceUsage = true, true
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		repairSource, repairTarget = previousSource, previousTarget
		repair.SilenceErrors, repair.SilenceUsage = previousSilenceErrors, previousSilenceUsage
		apply := repair.Flags().Lookup("apply")
		_ = apply.Value.Set("false")
		for _, flag := range []string{"source", "target", "apply"} {
			repair.Flags().Lookup(flag).Changed = false
		}
	})
	return func(args ...string) (string, error) {
		t.Helper()
		rootCmd.SetArgs(append([]string{"repair", "--source", "test-account", "--target", "local"}, args...))
		return captureOutput(t, func() error { return rootCmd.ExecuteContext(t.Context()) })
	}
}

func assertVerifyReport(t *testing.T, want verificationReport) {
	t.Helper()
	cmd := newVerifyCommand()
	out, err := executeBackupCommand(t, cmd, "local")
	var got verificationReport
	decodeErr := json.Unmarshal(out, &got)
	if decodeErr != nil || got != want || (err != nil) != (want.Corrupt+want.Missing > 0) {
		t.Fatalf("verify = %+v, error = %v, decode = %v; want %+v", got, err, decodeErr, want)
	}
}

func assertRepairStats(t *testing.T, output string, err error) {
	t.Helper()
	var stats mailvaultsync.SyncStats
	decodeErr := json.Unmarshal([]byte(output), &stats)
	if err != nil || decodeErr != nil || stats.TotalWritten != 2 || stats.TotalErrors != 0 {
		t.Fatalf("repair output = %s, error = %v, decode = %v", output, err, decodeErr)
	}
}

func assertRepairedLocators(t *testing.T, original, repaired map[string]string) {
	t.Helper()
	if repaired["healthy"] != original["healthy"] || repaired["corrupt"] == original["corrupt"] || repaired["missing"] == original["missing"] {
		t.Fatalf("unexpected locator changes: before=%v after=%v", original, repaired)
	}
}

func TestSyncDryRunAndRepairWithMaildir(t *testing.T) {
	messages := syncBackupMessages()
	fixture := setupSyncBackupFixture(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	backup, err := builtin.Registry().CreateTarget(cfg.Targets[0].Type, config.TargetPluginConfig(cfg.Targets[0], cfg.MaxMessageBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	dryRun = true
	runSyncExpectBackup(t, 3, 3)
	assertDryRunCreatedNothing(t, fixture.targetPath, fixture.dbPath)
	dryRun = false
	runSyncExpectBackup(t, 3, 3)
	original := manifestLocators(t, fixture, backup, messages)
	assertBackupContents(t, original, messages, "backup")
	damageBackupMessages(t, original)
	runRepair := armRepairCommand(t)
	before := readCommandFixture(t, fixture.dbPath)
	if _, err := runRepair(); err == nil || !strings.Contains(err.Error(), "pass --apply") {
		t.Fatalf("repair without confirmation: %v", err)
	}
	assertDamagedCopies(t, original)
	assertVerifyReport(t, verificationReport{Target: "local", Scanned: 2, Verified: 1, Corrupt: 1, Missing: 1})
	if !bytes.Equal(before, readCommandFixture(t, fixture.dbPath)) {
		t.Fatal("verification or repair without --apply changed the state database")
	}
	output, err := runRepair("--apply")
	assertRepairStats(t, output, err)
	repaired := manifestLocators(t, fixture, backup, messages)
	assertRepairedLocators(t, original, repaired)
	assertDamagedCopies(t, original)
	assertBackupContents(t, repaired, messages, "repaired")
	assertVerifyReport(t, verificationReport{Target: "local", Scanned: 4, Verified: 3})
}

func executeBackupCommand(t *testing.T, cmd *cobra.Command, args ...string) ([]byte, error) {
	t.Helper()
	var output bytes.Buffer
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&output)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return output.Bytes(), err
}

func readCommandFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
