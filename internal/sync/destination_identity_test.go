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

type destinationFixture struct {
	store        *state.Store
	cfg          *config.Config
	registry     *plugins.Registry
	targetConfig map[string]any
	pathA        string
	pathB        string
	pathC        string
}

func setupDestinationFixture(t *testing.T) *destinationFixture {
	t.Helper()
	ctx := t.Context()
	store, err := state.NewStore(t.Context(), filepath.Join(physicalTempDir(t), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	root := physicalTempDir(t)
	raw := []byte("Subject: saved\r\n\r\nmessage\r\n")
	hash := dedup.ComputeContentHash(raw)
	source := &knownUIDFixtureSource{message: &plugins.Message{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: raw}}
	targetConfig := map[string]any{"path": filepath.Join(root, "a")}
	cfg := &config.Config{
		Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"INBOX"}}},
		Targets: []config.TargetConfig{{Name: "archive", Type: "maildir", Config: targetConfig}},
		Pairs:   []config.PairConfig{{Source: "account", Targets: []string{"archive"}}},
	}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]any) (plugins.Source, error) { return source, nil })
	registry.RegisterTarget("maildir", target.NewMaildirTarget)
	if err := store.MarkDelivered(ctx, "archive", "hash:"+hash, "account", "INBOX", "1"); err != nil {
		t.Fatal(err)
	}
	legacy := plugins.BackupMessage{Source: "account", Target: "archive", Scope: "generation", Folder: "INBOX", UID: "1", Locator: "old-location", RawSHA256: hash}
	if err := store.RecordBackupMessage(ctx, &legacy); err != nil {
		t.Fatal(err)
	}
	return &destinationFixture{store: store, cfg: cfg, registry: registry, targetConfig: targetConfig, pathA: filepath.Join(root, "a"), pathB: filepath.Join(root, "b"), pathC: filepath.Join(root, "c")}
}

func (fixture *destinationFixture) run(t *testing.T, options ...SyncOption) *SyncStats {
	t.Helper()
	engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry, options...)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := engine.Run(t.Context())
	if err != nil || stats.TotalErrors != 0 {
		t.Fatalf("run: %+v %v", stats, err)
	}
	return stats
}

func storedMessageCount(t *testing.T, path string) int {
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

func assertSameDestinationReceipts(t *testing.T, fixture *destinationFixture) {
	t.Helper()
	if stats := fixture.run(t); stats.TotalWritten != 1 {
		t.Fatalf("initial write: %+v", stats)
	}
	if stats := fixture.run(t); stats.TotalFetched != 0 || stats.TotalWritten != 0 {
		t.Fatalf("same destination did not use incremental receipts: %+v", stats)
	}
}

func loadManifest(t *testing.T, fixture *destinationFixture, path string) []plugins.BackupMessage {
	t.Helper()
	loaded, _ := target.NewMaildirTarget(map[string]any{"path": path})
	mustAssert[interface{ SetName(name string) }](t, loaded).SetName("archive")
	manifest, err := fixture.store.BackupMessages(t.Context(), plugins.TargetStateKey(loaded))
	if err != nil || len(manifest) != 1 {
		t.Fatalf("destination manifest: entries=%d err=%v", len(manifest), err)
	}
	return manifest
}

func assertNewDestination(t *testing.T, fixture *destinationFixture, oldManifest []plugins.BackupMessage) {
	t.Helper()
	fixture.targetConfig["path"] = fixture.pathB
	if stats := fixture.run(t); stats.TotalFetched != 1 || stats.TotalWritten != 1 {
		t.Fatalf("new destination reused old receipt: %+v", stats)
	}
	if storedMessageCount(t, fixture.pathA) != 1 || storedMessageCount(t, fixture.pathB) != 1 {
		t.Fatal("messages were not stored once at each configured destination")
	}
	newManifest := loadManifest(t, fixture, fixture.pathB)
	if newManifest[0].Locator == oldManifest[0].Locator {
		t.Fatalf("new destination manifest reused old locator: old=%+v new=%+v", oldManifest, newManifest)
	}
}

func assertForceFetchAllDestination(t *testing.T, fixture *destinationFixture) {
	t.Helper()
	fixture.targetConfig["path"] = fixture.pathC
	if stats := fixture.run(t, WithForceFetchAll(true)); stats.TotalFetched != 1 || stats.TotalWritten != 1 {
		t.Fatalf("force-fetch-all reused old destination receipt: %+v", stats)
	}
	if storedMessageCount(t, fixture.pathC) != 1 {
		t.Fatal("force-fetch-all did not write to the reconfigured destination")
	}
	legacyManifest, err := fixture.store.BackupMessages(t.Context(), "archive")
	if err != nil || len(legacyManifest) != 1 || legacyManifest[0].Locator != "old-location" {
		t.Fatalf("legacy manifest was changed while adding location-scoped state: entries=%+v err=%v", legacyManifest, err)
	}
}

func TestMaildirDestinationChangeDoesNotReuseReceiptsOrManifest(t *testing.T) {
	fixture := setupDestinationFixture(t)
	assertSameDestinationReceipts(t, fixture)
	oldManifest := loadManifest(t, fixture, fixture.pathA)
	assertNewDestination(t, fixture, oldManifest)
	assertForceFetchAllDestination(t, fixture)
}
