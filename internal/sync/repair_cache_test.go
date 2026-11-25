package sync

import (
	"bytes"
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

type aliasFixture struct {
	root     string
	store    *state.Store
	cfg      *config.Config
	registry *plugins.Registry
	stateKey string
}

func setupAliasFixture(t *testing.T) *aliasFixture {
	t.Helper()
	root := physicalTempDir(t)
	store, err := state.NewStore(t.Context(), filepath.Join(physicalTempDir(t), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	raw := []byte("Subject: duplicate\r\n\r\none message\r\n")
	sources := map[string]*fixtureSource{
		"account-a": {name: "account-a", messages: []*plugins.Message{{UID: "1", UIDScope: "generation-a", Folder: "INBOX", Raw: raw}}},
		"account-b": {name: "account-b", messages: []*plugins.Message{{UID: "9", UIDScope: "generation-b", Folder: "INBOX", Raw: raw}}},
	}
	cfg := &config.Config{
		Sources: []config.SourceConfig{{Name: "account-a", Type: "fixture", Folders: []string{"INBOX"}}, {Name: "account-b", Type: "fixture", Folders: []string{"INBOX"}}},
		Targets: []config.TargetConfig{{Name: "local", Type: "maildir", Config: map[string]any{"path": root, "max_listing_entries": 64, "max_listing_locator_bytes": 4096}}},
		Pairs:   []config.PairConfig{{Source: "account-a", Targets: []string{"local"}}, {Source: "account-b", Targets: []string{"local"}}},
	}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(values map[string]any) (plugins.Source, error) {
		return sources[mustAssert[string](t, values["name"])], nil
	})
	registry.RegisterTarget("maildir", target.NewMaildirTarget)
	stateTarget, err := target.NewMaildirTarget(map[string]any{"path": root})
	if err != nil {
		t.Fatal(err)
	}
	mustAssert[interface{ SetName(name string) }](t, stateTarget).SetName("local")
	return &aliasFixture{root: root, store: store, cfg: cfg, registry: registry, stateKey: plugins.TargetStateKey(stateTarget)}
}

func (fixture *aliasFixture) run(t *testing.T, options ...SyncOption) *SyncStats {
	t.Helper()
	engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry, options...)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := engine.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func (fixture *aliasFixture) corruptOriginal(t *testing.T) (string, string) {
	t.Helper()
	entries, err := fixture.store.BackupMessages(t.Context(), fixture.stateKey)
	if err != nil || len(entries) != 2 || entries[0].Locator != entries[1].Locator {
		t.Fatalf("initial aliases: %+v, %v", entries, err)
	}
	originalLocator := entries[0].Locator
	original := filepath.Join(fixture.root, filepath.FromSlash(originalLocator))
	moved := filepath.Join(fixture.root, "cur", filepath.Base(original))
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	original = moved
	if err := os.WriteFile(original, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	return original, originalLocator
}

func (fixture *aliasFixture) assertRepairedAliases(t *testing.T, original, originalLocator string, raw []byte) {
	t.Helper()
	entries, err := fixture.store.BackupMessages(t.Context(), fixture.stateKey)
	if err != nil || len(entries) != 2 || entries[0].Locator != entries[1].Locator || entries[0].Locator == originalLocator {
		t.Fatalf("repaired aliases: %+v, %v", entries, err)
	}
	replacement := filepath.Join(fixture.root, filepath.FromSlash(entries[0].Locator))
	if replacement == original {
		t.Fatal("repair overwrote the damaged object")
	}
	if data, err := os.ReadFile(replacement); err != nil || !bytes.Equal(data, raw) {
		t.Fatalf("replacement data = %q, %v", data, err)
	}
	assertAliasPhysicalCount(t, fixture, originalLocator)
}

func assertAliasPhysicalCount(t *testing.T, fixture *aliasFixture, originalLocator string) {
	t.Helper()
	maildir, err := target.NewMaildirTarget(map[string]any{"name": "local", "path": fixture.root})
	if err != nil {
		t.Fatal(err)
	}
	physical, err := mustAssert[plugins.TargetBackupReader](t, maildir).ListBackupMessages(t.Context())
	if err != nil || len(physical) != 2 {
		t.Fatalf("physical Maildir objects = %d, %v; want damaged original and one replacement", len(physical), err)
	}
	superseded, err := fixture.store.SupersededBackupObjects(t.Context(), fixture.stateKey)
	if err != nil || !superseded[originalLocator] {
		t.Fatalf("superseded original = %#v, %v", superseded, err)
	}
}

func TestRepairReusesReplacementAcrossSourceAliases(t *testing.T) {
	fixture := setupAliasFixture(t)
	raw := []byte("Subject: duplicate\r\n\r\none message\r\n")
	if stats := fixture.run(t); stats.TotalErrors != 0 || stats.TotalWritten != 1 {
		t.Fatalf("initial sync: %+v", stats)
	}
	original, originalLocator := fixture.corruptOriginal(t)
	stats := fixture.run(t, WithRepair(true))
	if stats.TotalErrors != 0 || stats.TotalWritten != 1 {
		t.Fatalf("repair aliases: %+v", stats)
	}
	fixture.assertRepairedAliases(t, original, originalLocator, raw)
	if stats := fixture.run(t, WithRepair(true)); stats.TotalErrors != 0 || stats.TotalWritten != 0 {
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
	store, err := state.NewStore(t.Context(), filepath.Join(physicalTempDir(t), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw := []byte("Subject: retry\r\n\r\nretry body\r\n") //nolint:dupword // fixture message content, not prose
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
	registry.RegisterSource("fixture", func(map[string]any) (plugins.Source, error) { return source, nil })
	registry.RegisterTarget("repair-failure", func(map[string]any) (plugins.Target, error) { return backup, nil })
	engine, err := NewSyncEngine(cfg, store, registry, WithRepair(true), WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := engine.Run(t.Context())
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
	repairTarget := &repairIndexTarget{name: "target", entry: plugins.BackupMessage{Locator: "old-locator", Folder: "INBOX"}, raw: oldRaw}
	engine := &SyncEngine{}
	msg := &plugins.Message{Folder: "INBOX"}
	engine.rememberRepairBackup(repairTarget, msg, "new-hash", "new-locator")
	if _, loaded := engine.repairCache[repairTarget.Name()]; loaded {
		t.Fatal("remembered one write as a complete target index")
	}
	locator, found, err := engine.findBackupByHash(t.Context(), repairTarget, msg, dedup.ComputeContentHash(oldRaw))
	if err != nil || !found || locator != "old-locator" || repairTarget.listCalls != 1 {
		t.Fatalf("scan result = %q, %t, %v; list calls = %d", locator, found, err, repairTarget.listCalls)
	}
}

func TestRepairCacheKeepsPreservedFoldersDistinct(t *testing.T) {
	maildir, err := target.NewMaildirTarget(map[string]any{"path": physicalTempDir(t), "preserve_folders": true})
	if err != nil {
		t.Fatal(err)
	}
	mustAssert[interface{ SetName(name string) }](t, maildir).SetName("local")
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

type folderRepairFixture struct {
	root     string
	store    *state.Store
	cfg      *config.Config
	registry *plugins.Registry
	stateKey string
}

func setupFolderRepairFixture(t *testing.T) *folderRepairFixture {
	t.Helper()
	root := physicalTempDir(t)
	store, err := state.NewStore(t.Context(), filepath.Join(physicalTempDir(t), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	raw := []byte("Subject: folders\r\n\r\nsame bytes\r\n")
	source := &fixtureSource{name: "account", messages: []*plugins.Message{
		{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: raw},
		{UID: "2", UIDScope: "generation", Folder: "Sent", Raw: raw},
	}}
	cfg := &config.Config{
		Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"INBOX", "Sent"}}},
		Targets: []config.TargetConfig{{Name: "local", Type: "maildir", Config: map[string]any{"path": root, "preserve_folders": true}}},
		Pairs:   []config.PairConfig{{Source: "account", Targets: []string{"local"}}},
	}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]any) (plugins.Source, error) { return source, nil })
	registry.RegisterTarget("maildir", target.NewMaildirTarget)
	stateTarget, err := target.NewMaildirTarget(map[string]any{"path": root})
	if err != nil {
		t.Fatal(err)
	}
	mustAssert[interface{ SetName(name string) }](t, stateTarget).SetName("local")
	return &folderRepairFixture{root: root, store: store, cfg: cfg, registry: registry, stateKey: plugins.TargetStateKey(stateTarget)}
}

func (fixture *folderRepairFixture) run(t *testing.T, options ...SyncOption) *SyncStats {
	t.Helper()
	engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry, options...)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := engine.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func (fixture *folderRepairFixture) corruptEntries(t *testing.T) {
	t.Helper()
	entries, err := fixture.store.BackupMessages(t.Context(), fixture.stateKey)
	if err != nil || len(entries) != 2 || entries[0].Locator == entries[1].Locator {
		t.Fatalf("folder manifests: %+v, %v", entries, err)
	}
	for i := range entries {
		path := filepath.Join(fixture.root, filepath.FromSlash(entries[i].Locator))
		if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func (fixture *folderRepairFixture) assertRepairedEntries(t *testing.T) {
	t.Helper()
	entries, err := fixture.store.BackupMessages(t.Context(), fixture.stateKey)
	if err != nil || len(entries) != 2 || entries[0].Locator == entries[1].Locator {
		t.Fatalf("repaired folder manifests: %+v, %v", entries, err)
	}
}

func TestRepairWritesOneReplacementPerPreservedFolder(t *testing.T) {
	fixture := setupFolderRepairFixture(t)
	if stats := fixture.run(t); stats.TotalErrors != 0 || stats.TotalWritten != 2 {
		t.Fatalf("initial sync: %+v", stats)
	}
	fixture.corruptEntries(t)
	if stats := fixture.run(t, WithRepair(true)); stats.TotalErrors != 0 || stats.TotalWritten != 2 {
		t.Fatalf("repair folders: %+v", stats)
	}
	fixture.assertRepairedEntries(t)
}
