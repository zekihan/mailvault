package main

import (
	"bytes"
	"context"
	"github.com/zekihan/mailvault/internal/state"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDryRunCreatesNeitherTargetNorState(t *testing.T) {
	configureCLI(t)
	dryRun = true
	jsonOutput = false
	stateDir = filepath.Join(t.TempDir(), "missing", "state")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Targets[0].Config["path"].(string)
	output, err := captureOutput(t, func() error { return runSync(nil, nil) })
	if err == nil || !strings.Contains(output, "ERROR") {
		t.Fatalf("expected refused source with diagnostics: %v %s", err, output)
	}
	for _, p := range []string{path, stateDir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("dry run created %s: %v", p, err)
		}
	}
}
func TestDryRunDoesNotModifyExistingState(t *testing.T) {
	configureCLI(t)
	dryRun = true
	jsonOutput = false
	path := filepath.Join(stateDir, "state.db")
	store, err := state.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSource(context.Background(), "test-account", "pop3", "original-host", "original-user"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = captureOutput(t, func() error { return runSync(nil, nil) })
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("dry run modified existing SQLite state")
	}
}
