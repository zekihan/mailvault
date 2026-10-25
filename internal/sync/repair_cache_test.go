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

func TestRepairReusesReplacementAcrossSourceAliases(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw := []byte("Subject: duplicate\r\n\r\none message\r\n")
	sources := map[string]*fixtureSource{
		"account-a": {name: "account-a", messages: []*plugins.Message{{UID: "1", UIDScope: "generation-a", Folder: "INBOX", Raw: raw}}},
		"account-b": {name: "account-b", messages: []*plugins.Message{{UID: "9", UIDScope: "generation-b", Folder: "INBOX", Raw: raw}}},
	}
	cfg := &config.Config{
		Sources: []config.SourceConfig{{Name: "account-a", Type: "fixture", Folders: []string{"INBOX"}}, {Name: "account-b", Type: "fixture", Folders: []string{"INBOX"}}},
		Targets: []config.TargetConfig{{Name: "local", Type: "maildir", Config: map[string]interface{}{"path": root, "max_listing_entries": 64, "max_listing_locator_bytes": 4096}}},
		Pairs:   []config.PairConfig{{Source: "account-a", Targets: []string{"local"}}, {Source: "account-b", Targets: []string{"local"}}},
	}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(values map[string]interface{}) (plugins.Source, error) {
		return sources[values["name"].(string)], nil
	})
	registry.RegisterTarget("maildir", target.NewMaildirTarget)
	stateTarget, err := target.NewMaildirTarget(map[string]interface{}{"path": root})
	if err != nil {
		t.Fatal(err)
	}
	stateTarget.(interface{ SetName(string) }).SetName("local")
	stateKey := plugins.TargetStateKey(stateTarget)
	run := func(options ...SyncOption) *SyncStats {
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
	if stats := run(); stats.TotalErrors != 0 || stats.TotalWritten != 1 {
		t.Fatalf("initial sync: %+v", stats)
	}
	entries, err := store.BackupMessages(ctx, stateKey)
	if err != nil || len(entries) != 2 || entries[0].Locator != entries[1].Locator {
		t.Fatalf("initial aliases: %+v, %v", entries, err)
	}
	originalLocator := entries[0].Locator
	original := filepath.Join(root, filepath.FromSlash(originalLocator))
	moved := filepath.Join(root, "cur", filepath.Base(original))
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	original = moved
	if err := os.WriteFile(original, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	stats := run(WithRepair(true))
	if stats.TotalErrors != 0 || stats.TotalWritten != 1 {
		t.Fatalf("repair aliases: %+v", stats)
	}
	entries, err = store.BackupMessages(ctx, stateKey)
	if err != nil || len(entries) != 2 || entries[0].Locator != entries[1].Locator || entries[0].Locator == originalLocator {
		t.Fatalf("repaired aliases: %+v, %v", entries, err)
	}
	// Both alias records point at the single replacement, while the damaged
	// original remains present as a superseded physical object.
	replacement := filepath.Join(root, filepath.FromSlash(entries[0].Locator))
	if replacement == original {
		t.Fatal("repair overwrote the damaged object")
	}
	if data, err := os.ReadFile(replacement); err != nil || string(data) != string(raw) {
		t.Fatalf("replacement data = %q, %v", data, err)
	}
	maildir, err := target.NewMaildirTarget(map[string]interface{}{"name": "local", "path": root})
	if err != nil {
		t.Fatal(err)
	}
	physical, err := maildir.(plugins.TargetBackupReader).ListBackupMessages(ctx)
	if err != nil || len(physical) != 2 {
		t.Fatalf("physical Maildir objects = %d, %v; want damaged original and one replacement", len(physical), err)
	}
	superseded, err := store.SupersededBackupObjects(ctx, stateKey)
	if err != nil || !superseded[originalLocator] {
		t.Fatalf("superseded original = %#v, %v", superseded, err)
	}
	if stats := run(WithRepair(true)); stats.TotalErrors != 0 || stats.TotalWritten != 0 {
		t.Fatalf("repeat repair: %+v", stats)
	}
}

type repairWriteFailureTarget struct {
	name       string
	writeCalls int
	listCalls  int
}

func (t *repairWriteFailureTarget) Name() string                                       { return t.name }
func (*repairWriteFailureTarget) Type() string                                         { return "repair-failure" }
func (*repairWriteFailureTarget) Initialize(context.Context) error                     { return nil }
func (*repairWriteFailureTarget) Close() error                                         { return nil }
func (*repairWriteFailureTarget) WriteMessage(context.Context, *plugins.Message) error { return nil }
func (t *repairWriteFailureTarget) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	t.listCalls++
	return nil, nil
}
func (*repairWriteFailureTarget) ReadBackupMessage(context.Context, string) (*plugins.Message, error) {
	return nil, plugins.ErrBackupMessageMissing
}
func (t *repairWriteFailureTarget) WriteRepairMessage(context.Context, *plugins.Message) (string, error) {
	t.writeCalls++
	return "", errFixture
}

func TestRepairWriteFailuresDoNotPopulateCache(t *testing.T) {
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw := []byte("Subject: retry\r\n\r\nretry body\r\n")
	source := &fixtureSource{name: "account", messages: []*plugins.Message{
		{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: raw},
		{UID: "2", UIDScope: "generation", Folder: "INBOX", Raw: raw},
	}}
	backup := &repairWriteFailureTarget{name: "target"}
	cfg := &config.Config{
		Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"INBOX"}}},
		Targets: []config.TargetConfig{{Name: "target", Type: "repair-failure"}},
		Pairs:   []config.PairConfig{{Source: "account", Targets: []string{"target"}}},
	}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]interface{}) (plugins.Source, error) { return source, nil })
	registry.RegisterTarget("repair-failure", func(map[string]interface{}) (plugins.Target, error) { return backup, nil })
	engine, err := NewSyncEngine(cfg, store, registry, WithRepair(true), WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := engine.Run(context.Background())
	if err != nil || stats.TotalWritten != 0 || stats.TotalErrors != 2 {
		t.Fatalf("repair result: %+v, %v", stats, err)
	}
	if backup.writeCalls != 2 {
		t.Fatalf("repair writes = %d, want 2; failed write created a false cache hit", backup.writeCalls)
	}
}

type repairIndexTarget struct {
	name      string
	listCalls int
	entry     plugins.BackupMessage
	raw       []byte
}

func (t *repairIndexTarget) Name() string                                       { return t.name }
func (*repairIndexTarget) Type() string                                         { return "repair-index" }
func (*repairIndexTarget) Initialize(context.Context) error                     { return nil }
func (*repairIndexTarget) Close() error                                         { return nil }
func (*repairIndexTarget) WriteMessage(context.Context, *plugins.Message) error { return nil }
func (t *repairIndexTarget) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	t.listCalls++
	return []plugins.BackupMessage{t.entry}, nil
}
func (t *repairIndexTarget) ReadBackupMessage(_ context.Context, locator string) (*plugins.Message, error) {
	if locator != t.entry.Locator {
		return nil, plugins.ErrBackupMessageMissing
	}
	return &plugins.Message{Folder: t.entry.Folder, Raw: t.raw}, nil
}

func TestRememberRepairBackupDoesNotCreatePartialIndex(t *testing.T) {
	oldRaw := []byte("Subject: existing\r\n\r\nold backup\r\n")
	target := &repairIndexTarget{name: "target", entry: plugins.BackupMessage{Locator: "old-locator", Folder: "INBOX"}, raw: oldRaw}
	engine := &SyncEngine{}
	msg := &plugins.Message{Folder: "INBOX"}
	engine.rememberRepairBackup(target, msg, "new-hash", "new-locator")
	if _, loaded := engine.repairCache[target.Name()]; loaded {
		t.Fatal("remembered one write as a complete target index")
	}
	locator, found, err := engine.findBackupByHash(context.Background(), target, msg, dedup.ComputeContentHash(oldRaw))
	if err != nil || !found || locator != "old-locator" || target.listCalls != 1 {
		t.Fatalf("scan result = %q, %t, %v; list calls = %d", locator, found, err, target.listCalls)
	}
}

func TestRepairCacheKeepsPreservedFoldersDistinct(t *testing.T) {
	maildir, err := target.NewMaildirTarget(map[string]interface{}{"path": t.TempDir(), "preserve_folders": true})
	if err != nil {
		t.Fatal(err)
	}
	maildir.(interface{ SetName(string) }).SetName("local")
	engine := &SyncEngine{}
	index := map[string]string{}
	engine.repairCache = map[string]map[string]string{maildir.Name(): index}
	first := &plugins.Message{Folder: "INBOX"}
	second := &plugins.Message{Folder: "Sent"}
	engine.rememberRepairBackup(maildir, first, "hash", "first-locator")
	engine.rememberRepairBackup(maildir, second, "hash", "second-locator")
	if len(index) != 2 {
		t.Fatalf("folder identities collapsed: %#v", index)
	}
	if index[targetDeliveryIdentity(maildir, first, "hash:hash")] != "first-locator" || index[targetDeliveryIdentity(maildir, second, "hash:hash")] != "second-locator" {
		t.Fatalf("wrong folder cache values: %#v", index)
	}
}

func TestRepairWritesOneReplacementPerPreservedFolder(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw := []byte("Subject: folders\r\n\r\nsame bytes\r\n")
	source := &fixtureSource{name: "account", messages: []*plugins.Message{
		{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: raw},
		{UID: "2", UIDScope: "generation", Folder: "Sent", Raw: raw},
	}}
	cfg := &config.Config{
		Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"INBOX", "Sent"}}},
		Targets: []config.TargetConfig{{Name: "local", Type: "maildir", Config: map[string]interface{}{"path": root, "preserve_folders": true}}},
		Pairs:   []config.PairConfig{{Source: "account", Targets: []string{"local"}}},
	}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]interface{}) (plugins.Source, error) { return source, nil })
	registry.RegisterTarget("maildir", target.NewMaildirTarget)
	stateTarget, err := target.NewMaildirTarget(map[string]interface{}{"path": root})
	if err != nil {
		t.Fatal(err)
	}
	stateTarget.(interface{ SetName(string) }).SetName("local")
	stateKey := plugins.TargetStateKey(stateTarget)
	run := func(options ...SyncOption) *SyncStats {
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
	if stats := run(); stats.TotalErrors != 0 || stats.TotalWritten != 2 {
		t.Fatalf("initial sync: %+v", stats)
	}
	entries, err := store.BackupMessages(ctx, stateKey)
	if err != nil || len(entries) != 2 || entries[0].Locator == entries[1].Locator {
		t.Fatalf("folder manifests: %+v, %v", entries, err)
	}
	for _, entry := range entries {
		path := filepath.Join(root, filepath.FromSlash(entry.Locator))
		if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if stats := run(WithRepair(true)); stats.TotalErrors != 0 || stats.TotalWritten != 2 {
		t.Fatalf("repair folders: %+v", stats)
	}
	entries, err = store.BackupMessages(ctx, stateKey)
	if err != nil || len(entries) != 2 || entries[0].Locator == entries[1].Locator {
		t.Fatalf("repaired folder manifests: %+v, %v", entries, err)
	}
}
