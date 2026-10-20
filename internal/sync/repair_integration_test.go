package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/state"
)

func TestRepairMissingAndCorruptMaildirRetainsOriginal(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw := []byte("Subject: original\r\n\r\ncomplete message\r\n")
	source := &fixtureSource{name: "account", messages: []*plugins.Message{{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: raw}}}
	cfg := &config.Config{Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"INBOX"}}}, Targets: []config.TargetConfig{{Name: "local", Type: "maildir", Config: map[string]interface{}{"path": root}}}, Pairs: []config.PairConfig{{Source: "account", Targets: []string{"local"}}}}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]interface{}) (plugins.Source, error) { return source, nil })
	registry.RegisterTarget("maildir", target.NewMaildirTarget)
	run := func(options ...SyncOption) *SyncStats {
		t.Helper()
		engine, err := NewSyncEngine(cfg, store, registry, options...)
		if err != nil {
			t.Fatal(err)
		}
		stats, err := engine.Run(ctx)
		if err != nil || stats.TotalErrors != 0 {
			t.Fatalf("run: %+v %v", stats, err)
		}
		return stats
	}
	locator := func() string {
		t.Helper()
		entries, err := store.BackupMessages(ctx, "local")
		if err != nil || len(entries) != 1 {
			t.Fatalf("manifest: %+v %v", entries, err)
		}
		return filepath.Join(root, filepath.FromSlash(entries[0].Locator))
	}
	run()
	first := locator()
	if err := os.WriteFile(first, []byte("corrupt original"), 0600); err != nil {
		t.Fatal(err)
	}
	if stats := run(WithRepair(true)); stats.TotalWritten != 1 {
		t.Fatalf("repair corrupt: %+v", stats)
	}
	if data, err := os.ReadFile(first); err != nil || string(data) != "corrupt original" {
		t.Fatal("corrupt original removed")
	}
	second := locator()
	if second == first {
		t.Fatal("repair overwrote original")
	}
	if data, err := os.ReadFile(second); err != nil || string(data) != string(raw) {
		t.Fatal("repair content mismatch")
	}
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	if stats := run(WithRepair(true)); stats.TotalWritten != 1 {
		t.Fatalf("repair missing: %+v", stats)
	}
	if stats := run(WithRepair(true)); stats.TotalWritten != 0 {
		t.Fatalf("healthy repair wrote copies: %+v", stats)
	}
	before := locator()
	run(WithDryRun(true), WithForceFetchAll(true))
	if locator() != before {
		t.Fatal("dry run changed manifest")
	}
}
