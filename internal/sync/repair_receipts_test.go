package sync

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/state"
	_ "modernc.org/sqlite"
)

func TestRepairAdoptionPersistsReceipts(t *testing.T) {
	for _, withUIDScope := range []bool{true, false} {
		name := "without-uid-scope"
		if withUIDScope {
			name = "with-uid-scope"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			statePath := filepath.Join(t.TempDir(), "state.db")
			message := &plugins.Message{UID: "42", Folder: "Archive", MessageID: "repair@example.invalid", Raw: []byte("Message-ID: <repair@example.invalid>\r\n\r\nbody\r\n")}
			if withUIDScope {
				message.UIDScope = "generation-1"
			}
			message.ContentHash = dedup.ComputeContentHash(message.Raw)
			source := &fixtureSource{name: "account", messages: []*plugins.Message{message}}
			var registeredSource plugins.Source = source
			cfg, registry := repairReceiptFixture(root, &registeredSource)

			initialStore := openReceiptTestStore(t, statePath)
			runReceiptTestEngine(t, ctx, cfg, initialStore, registry)
			if err := initialStore.Close(); err != nil {
				t.Fatal(err)
			}

			store := openReceiptTestStore(t, filepath.Join(t.TempDir(), "fresh-state.db"))
			stats := runReceiptTestEngine(t, ctx, cfg, store, registry, WithRepair(true))
			if stats.TotalWritten != 0 || stats.TotalSkipped != 1 || stats.TotalErrors != 0 {
				t.Fatalf("adoption stats: %+v", stats)
			}
			assertRepairReceipts(t, ctx, store, root, message)

			stats = runReceiptTestEngine(t, ctx, cfg, store, registry, WithForceFetchAll(true))
			if stats.TotalWritten != 0 || stats.TotalSkipped != 1 || stats.TotalErrors != 0 {
				t.Fatalf("force-fetch-all stats: %+v", stats)
			}
			assertMaildirObjectCount(t, ctx, root, 1)

			if withUIDScope {
				registeredSource = &incrementalFixtureSource{fixtureSource: source}
				stats = runReceiptTestEngine(t, ctx, cfg, store, registry)
				if stats.TotalFetched != 0 || stats.TotalErrors != 0 {
					t.Fatalf("incremental stats: %+v", stats)
				}
				assertMaildirObjectCount(t, ctx, root, 1)
			}
		})
	}
}

func TestRepairHealthyManifestRestoresMissingReceipts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	message := &plugins.Message{UID: "42", UIDScope: "generation-1", Folder: "Archive", Raw: []byte("Subject: intact\r\n\r\nbody\r\n")}
	message.ContentHash = dedup.ComputeContentHash(message.Raw)
	source := &fixtureSource{name: "account", messages: []*plugins.Message{message}}
	var registeredSource plugins.Source = source
	cfg, registry := repairReceiptFixture(root, &registeredSource)
	store := openReceiptTestStore(t, statePath)
	runReceiptTestEngine(t, ctx, cfg, store, registry)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	deleteRepairReceipts(t, statePath)

	store = openReceiptTestStore(t, statePath)
	t.Cleanup(func() { _ = store.Close() })
	stats := runReceiptTestEngine(t, ctx, cfg, store, registry, WithRepair(true))
	if stats.TotalErrors != 0 || stats.TotalWritten != 0 || stats.TotalSkipped != 1 {
		t.Fatalf("healthy-manifest repair stats: %+v", stats)
	}
	assertRepairReceipts(t, ctx, store, root, message)
}

func TestRepairAdoptionReceiptFailureIsNotReportedAsSuccess(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	message := &plugins.Message{UID: "42", UIDScope: "generation-1", Folder: "Archive", Raw: []byte("Subject: existing\r\n\r\nbody\r\n")}
	message.ContentHash = dedup.ComputeContentHash(message.Raw)
	source := &fixtureSource{name: "account", messages: []*plugins.Message{message}}
	var registeredSource plugins.Source = source
	cfg, registry := repairReceiptFixture(root, &registeredSource)
	initialStore := openReceiptTestStore(t, statePath)
	runReceiptTestEngine(t, ctx, cfg, initialStore, registry)
	if err := initialStore.Close(); err != nil {
		t.Fatal(err)
	}
	deleteRepairReceipts(t, statePath)
	for _, test := range []struct {
		name string
		path string
	}{
		{name: "healthy-manifest", path: statePath},
		{name: "hash-scan", path: filepath.Join(t.TempDir(), "fresh-state.db")},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupStore := openReceiptTestStore(t, test.path)
			if err := setupStore.Close(); err != nil {
				t.Fatal(err)
			}
			addDeliveryFailureTrigger(t, test.path)
			store := openReceiptTestStore(t, test.path)
			stats := runReceiptTestEngine(t, ctx, cfg, store, registry, WithRepair(true))
			if stats.TotalErrors == 0 || stats.TotalSkipped != 0 || stats.TotalWritten != 0 {
				t.Fatalf("failed receipt was reported as successful adoption: %+v", stats)
			}
		})
	}
}

func TestRepairDoesNotSupersedeSameLocator(t *testing.T) {
	ctx := context.Background()
	store := openReceiptTestStore(t, filepath.Join(t.TempDir(), "state.db"))
	raw := []byte("Subject: unchanged locator\r\n\r\nbody\r\n")
	message := &plugins.Message{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: raw}
	hash := dedup.ComputeContentHash(raw)
	backup := &sameLocatorRepairTarget{name: "target", locator: "offset:0", message: message}
	if err := store.RecordBackupMessage(ctx, plugins.BackupMessage{Source: "account", Target: "target", Scope: "generation", Folder: "INBOX", UID: "1", Locator: backup.locator, RawSHA256: hash}); err != nil {
		t.Fatal(err)
	}
	source := &fixtureSource{name: "account", messages: []*plugins.Message{message}}
	cfg := &config.Config{
		Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"INBOX"}}},
		Targets: []config.TargetConfig{{Name: "target", Type: "same-locator"}},
		Pairs:   []config.PairConfig{{Source: "account", Targets: []string{"target"}}},
	}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]interface{}) (plugins.Source, error) { return source, nil })
	registry.RegisterTarget("same-locator", func(map[string]interface{}) (plugins.Target, error) { return backup, nil })
	stats := runReceiptTestEngine(t, ctx, cfg, store, registry, WithRepair(true))
	if stats.TotalWritten != 1 {
		t.Fatalf("repair stats: %+v", stats)
	}
	superseded, err := store.SupersededBackupObjects(ctx, "target")
	if err != nil || len(superseded) != 0 {
		t.Fatalf("same locator was marked superseded: %#v, %v", superseded, err)
	}
}

type sameLocatorRepairTarget struct {
	name    string
	locator string
	message *plugins.Message
}

func (t *sameLocatorRepairTarget) Name() string                                       { return t.name }
func (*sameLocatorRepairTarget) Type() string                                         { return "same-locator" }
func (*sameLocatorRepairTarget) Initialize(context.Context) error                     { return nil }
func (*sameLocatorRepairTarget) Close() error                                         { return nil }
func (*sameLocatorRepairTarget) WriteMessage(context.Context, *plugins.Message) error { return nil }
func (t *sameLocatorRepairTarget) WriteRepairMessage(context.Context, *plugins.Message) (string, error) {
	return t.locator, nil
}
func (t *sameLocatorRepairTarget) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	return []plugins.BackupMessage{{Locator: t.locator, Folder: t.message.Folder}}, nil
}
func (*sameLocatorRepairTarget) ReadBackupMessage(context.Context, string) (*plugins.Message, error) {
	return nil, plugins.ErrBackupMessageCorrupt
}

func repairReceiptFixture(root string, source *plugins.Source) (*config.Config, *plugins.Registry) {
	cfg := &config.Config{
		Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"Archive"}}},
		Targets: []config.TargetConfig{{Name: "local", Type: "maildir", Config: map[string]interface{}{"path": root}}},
		Pairs:   []config.PairConfig{{Source: "account", Targets: []string{"local"}}},
	}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]interface{}) (plugins.Source, error) { return *source, nil })
	registry.RegisterTarget("maildir", target.NewMaildirTarget)
	return cfg, registry
}

func openReceiptTestStore(t *testing.T, path string) *state.Store {
	t.Helper()
	store, err := state.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func runReceiptTestEngine(t *testing.T, ctx context.Context, cfg *config.Config, store *state.Store, registry *plugins.Registry, options ...SyncOption) *SyncStats {
	t.Helper()
	engine, err := NewSyncEngine(cfg, store, registry, options...)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := engine.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func assertRepairReceipts(t *testing.T, ctx context.Context, store *state.Store, root string, message *plugins.Message) {
	t.Helper()
	backup, err := target.NewMaildirTarget(map[string]interface{}{"path": root})
	if err != nil {
		t.Fatal(err)
	}
	backup.(interface{ SetName(string) }).SetName("local")
	hash := dedup.ComputeContentHash(message.Raw)
	delivered, err := store.DeliveryExists(ctx, plugins.TargetStateKey(backup), targetDeliveryIdentity(backup, message, "hash:"+hash))
	if err != nil || !delivered {
		t.Fatalf("delivery receipt = %v, %v", delivered, err)
	}
	fetched, err := store.MessageExists(ctx, "account", message.Folder, message.UID)
	if err != nil || !fetched {
		t.Fatalf("fetched receipt = %v, %v", fetched, err)
	}
	entries, err := store.BackupMessages(ctx, plugins.TargetStateKey(backup))
	if err != nil || len(entries) != 1 {
		t.Fatalf("backup manifest: %+v, %v", entries, err)
	}
	wantUID := message.UID
	if message.UIDScope == "" {
		wantUID = "content:" + hash
	}
	if entries[0].UID != wantUID {
		t.Fatalf("manifest UID = %q, want %q", entries[0].UID, wantUID)
	}
}

func assertMaildirObjectCount(t *testing.T, ctx context.Context, root string, want int) {
	t.Helper()
	backup, err := target.NewMaildirTarget(map[string]interface{}{"path": root})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := backup.(plugins.TargetBackupReader).ListBackupMessages(ctx)
	if err != nil || len(entries) != want {
		t.Fatalf("Maildir objects = %d, %v; want %d", len(entries), err, want)
	}
}

func deleteRepairReceipts(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM deliveries; DELETE FROM messages;"); err != nil {
		t.Fatal(err)
	}
}

func addDeliveryFailureTrigger(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TRIGGER fail_delivery_receipt BEFORE INSERT ON deliveries BEGIN SELECT RAISE(ABORT, 'forced delivery receipt failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
}
