package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/state"
)

type knownUIDFixtureSource struct{ message *plugins.Message }

func (*knownUIDFixtureSource) Name() string                  { return "account" }
func (*knownUIDFixtureSource) Type() string                  { return "fixture" }
func (*knownUIDFixtureSource) Connect(context.Context) error { return nil }
func (*knownUIDFixtureSource) Close() error                  { return nil }
func (*knownUIDFixtureSource) ListFolders(context.Context) ([]string, error) {
	return []string{"INBOX"}, nil
}
func (s *knownUIDFixtureSource) FetchMessages(ctx context.Context, _ []string, _, _ int64) (<-chan *plugins.Message, <-chan error) {
	messages := make(chan *plugins.Message, 1)
	known, err := plugins.KnownUIDs(ctx, "generation", "INBOX")
	if err == nil && !known[s.message.UID] {
		messages <- s.message
	}
	close(messages)
	errors := make(chan error, 1)
	if err != nil {
		errors <- err
	}
	close(errors)
	return messages, errors
}

func TestMaildirDestinationChangeDoesNotReuseReceiptsOrManifest(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	pathA := filepath.Join(root, "a")
	pathB := filepath.Join(root, "b")
	pathC := filepath.Join(root, "c")
	raw := []byte("Subject: saved\r\n\r\nmessage\r\n")
	hash := dedup.ComputeContentHash(raw)
	source := &knownUIDFixtureSource{message: &plugins.Message{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: raw}}
	targetConfig := map[string]interface{}{"path": pathA}
	cfg := &config.Config{
		Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"INBOX"}}},
		Targets: []config.TargetConfig{{Name: "archive", Type: "maildir", Config: targetConfig}},
		Pairs:   []config.PairConfig{{Source: "account", Targets: []string{"archive"}}},
	}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]interface{}) (plugins.Source, error) { return source, nil })
	registry.RegisterTarget("maildir", target.NewMaildirTarget)
	if err := store.MarkDelivered(ctx, "archive", "hash:"+hash, "account", "INBOX", "1"); err != nil {
		t.Fatal(err)
	}
	legacy := plugins.BackupMessage{Source: "account", Target: "archive", Scope: "generation", Folder: "INBOX", UID: "1", Locator: "old-location", RawSHA256: hash}
	if err := store.RecordBackupMessage(ctx, legacy); err != nil {
		t.Fatal(err)
	}
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
	storedCount := func(path string) int {
		t.Helper()
		count := 0
		for _, dir := range []string{"cur", "new"} {
			files, err := os.ReadDir(filepath.Join(path, dir))
			if err != nil {
				t.Fatal(err)
			}
			count += len(files)
		}
		return count
	}

	if stats := run(); stats.TotalWritten != 1 {
		t.Fatalf("initial write: %+v", stats)
	}
	if stats := run(); stats.TotalFetched != 0 || stats.TotalWritten != 0 {
		t.Fatalf("same destination did not use incremental receipts: %+v", stats)
	}
	oldTarget, _ := target.NewMaildirTarget(map[string]interface{}{"path": pathA})
	oldTarget.(interface{ SetName(string) }).SetName("archive")
	oldKey := plugins.TargetStateKey(oldTarget)
	oldManifest, err := store.BackupMessages(ctx, oldKey)
	if err != nil || len(oldManifest) != 1 {
		t.Fatalf("initial destination manifest: entries=%d err=%v", len(oldManifest), err)
	}

	targetConfig["path"] = pathB
	if stats := run(); stats.TotalFetched != 1 || stats.TotalWritten != 1 {
		t.Fatalf("new destination reused old receipt: %+v", stats)
	}
	if storedCount(pathA) != 1 || storedCount(pathB) != 1 {
		t.Fatal("messages were not stored once at each configured destination")
	}
	newTarget, _ := target.NewMaildirTarget(map[string]interface{}{"path": pathB})
	newTarget.(interface{ SetName(string) }).SetName("archive")
	newManifest, err := store.BackupMessages(ctx, plugins.TargetStateKey(newTarget))
	if err != nil || len(newManifest) != 1 || newManifest[0].Locator == oldManifest[0].Locator {
		t.Fatalf("new destination manifest reused old locator: old=%+v new=%+v err=%v", oldManifest, newManifest, err)
	}

	targetConfig["path"] = pathC
	if stats := run(WithForceFetchAll(true)); stats.TotalFetched != 1 || stats.TotalWritten != 1 {
		t.Fatalf("force-fetch-all reused old destination receipt: %+v", stats)
	}
	if storedCount(pathC) != 1 {
		t.Fatal("force-fetch-all did not write to the reconfigured destination")
	}
	legacyManifest, err := store.BackupMessages(ctx, "archive")
	if err != nil || len(legacyManifest) != 1 || legacyManifest[0].Locator != "old-location" {
		t.Fatalf("legacy manifest was changed while adding location-scoped state: entries=%+v err=%v", legacyManifest, err)
	}
}
