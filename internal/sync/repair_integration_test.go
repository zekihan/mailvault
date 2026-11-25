package sync

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/state"
)

type repairFixture struct {
	root     string
	store    *state.Store
	cfg      *config.Config
	registry *plugins.Registry
	source   *fixtureSource
}

func newRepairFixture(t *testing.T) *repairFixture {
	t.Helper()
	root := physicalTempDir(t)
	store, err := state.NewStore(t.Context(), filepath.Join(physicalTempDir(t), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	raw := []byte("Subject: original\r\n\r\ncomplete message\r\n")
	source := &fixtureSource{name: "account", messages: []*plugins.Message{{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: raw}}}
	cfg := &config.Config{Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"INBOX"}}}, Targets: []config.TargetConfig{{Name: "local", Type: "maildir", Config: map[string]any{"path": root}}}, Pairs: []config.PairConfig{{Source: "account", Targets: []string{"local"}}}}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]any) (plugins.Source, error) { return source, nil })
	registry.RegisterTarget("maildir", target.NewMaildirTarget)
	return &repairFixture{root: root, store: store, cfg: cfg, registry: registry, source: source}
}

func (fixture *repairFixture) run(t *testing.T, options ...SyncOption) *SyncStats {
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

func (fixture *repairFixture) locator(t *testing.T) string {
	t.Helper()
	currentTarget, createErr := target.NewMaildirTarget(fixture.cfg.Targets[0].Config)
	if createErr != nil {
		t.Fatal(createErr)
	}
	mustAssert[interface{ SetName(name string) }](t, currentTarget).SetName("local")
	entries, err := fixture.store.BackupMessages(t.Context(), plugins.TargetStateKey(currentTarget))
	if err != nil || len(entries) != 1 {
		t.Fatalf("manifest: %+v %v", entries, err)
	}
	return filepath.Join(fixture.root, filepath.FromSlash(entries[0].Locator))
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatal("file content mismatch")
	}
}

func (fixture *repairFixture) repairCorrupt(t *testing.T, raw []byte) string {
	t.Helper()
	first := fixture.locator(t)
	if err := os.WriteFile(first, []byte("corrupt original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stats := fixture.run(t, WithRepair(true)); stats.TotalWritten != 1 {
		t.Fatalf("repair corrupt: %+v", stats)
	}
	assertFileContent(t, first, "corrupt original")
	second := fixture.locator(t)
	if second == first {
		t.Fatal("repair overwrote original")
	}
	if data, err := os.ReadFile(second); err != nil || !bytes.Equal(data, raw) {
		t.Fatal("repair content mismatch")
	}
	return second
}

func (fixture *repairFixture) repairMissingThenHealthy(t *testing.T) {
	t.Helper()
	if err := os.Remove(fixture.locator(t)); err != nil {
		t.Fatal(err)
	}
	if stats := fixture.run(t, WithRepair(true)); stats.TotalWritten != 1 {
		t.Fatalf("repair missing: %+v", stats)
	}
	if stats := fixture.run(t, WithRepair(true)); stats.TotalWritten != 0 {
		t.Fatalf("healthy repair wrote copies: %+v", stats)
	}
}

func (fixture *repairFixture) assertDryRunStable(t *testing.T) {
	t.Helper()
	before := fixture.locator(t)
	fixture.run(t, WithDryRun(true), WithForceFetchAll(true))
	if fixture.locator(t) != before {
		t.Fatal("dry run changed manifest")
	}
	requireSourceLossDetected(t, fixture, before)
}

func requireSourceLossDetected(t *testing.T, fixture *repairFixture, path string) {
	t.Helper()
	fixture.source.messages = nil
	if err := os.WriteFile(path, []byte("lost source copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry, WithRepair(true))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := engine.Run(t.Context())
	if err != nil || stats.TotalErrors == 0 {
		t.Fatalf("missing source falsely repaired: %+v %v", stats, err)
	}
}

func TestRepairMissingAndCorruptMaildirRetainsOriginal(t *testing.T) {
	fixture := newRepairFixture(t)
	raw := []byte("Subject: original\r\n\r\ncomplete message\r\n")
	fixture.run(t)
	fixture.repairCorrupt(t, raw)
	fixture.repairMissingThenHealthy(t)
	fixture.assertDryRunStable(t)
}

func runRepairEngine(ctx context.Context, t *testing.T, engine *SyncEngine, wantWritten int) {
	t.Helper()
	stats, err := engine.Run(ctx)
	if err != nil || stats.TotalErrors != 0 || stats.TotalWritten != wantWritten {
		t.Fatalf("repair run: %+v, %v; want %d writes", stats, err, wantWritten)
	}
}

func repairCacheLocators(ctx context.Context, t *testing.T, store *state.Store, maildir plugins.Target) []string {
	t.Helper()
	entries, err := store.BackupMessages(ctx, plugins.TargetStateKey(maildir))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(entries))
	for i := range entries {
		out[i] = entries[i].Locator
	}
	return out
}

func assertCacheReplacement(t *testing.T, root string, entries []string, original, originalPath string, raw []byte) {
	t.Helper()
	if len(entries) != 1 || entries[0] == original {
		t.Fatalf("manifest after repair = %v, original locator %q", entries, original)
	}
	if data, err := os.ReadFile(originalPath); err != nil || string(data) != "corrupt original" {
		t.Fatalf("corrupt original changed: %q, %v", data, err)
	}
	replacementPath := filepath.Join(root, filepath.FromSlash(entries[0]))
	if data, err := os.ReadFile(replacementPath); err != nil || !bytes.Equal(data, raw) {
		t.Fatalf("replacement data = %q, %v", data, err)
	}
}

func TestRepairCacheRefreshesAcrossRuns(t *testing.T) {
	root := physicalTempDir(t)
	ctx := t.Context()
	store, err := state.NewStore(t.Context(), filepath.Join(physicalTempDir(t), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw := []byte("Subject: original\r\n\r\ncomplete message\r\n")
	source := &fixtureSource{name: "account", messages: []*plugins.Message{{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: raw}}}
	cfg := &config.Config{Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"INBOX"}}}, Targets: []config.TargetConfig{{Name: "local", Type: "maildir", Config: map[string]any{"path": root}}}, Pairs: []config.PairConfig{{Source: "account", Targets: []string{"local"}}}}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]any) (plugins.Source, error) { return source, nil })
	registry.RegisterTarget("maildir", target.NewMaildirTarget)
	engine, err := NewSyncEngine(cfg, store, registry, WithRepair(true))
	if err != nil {
		t.Fatal(err)
	}
	maildir, err := target.NewMaildirTarget(map[string]any{"path": root})
	if err != nil {
		t.Fatal(err)
	}
	mustAssert[interface{ SetName(name string) }](t, maildir).SetName("local")
	runRepairEngine(ctx, t, engine, 1)
	original := repairCacheLocators(ctx, t, store, maildir)[0]
	originalPath := filepath.Join(root, filepath.FromSlash(original))
	if err := os.WriteFile(originalPath, []byte("corrupt original"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRepairEngine(ctx, t, engine, 1)
	assertCacheReplacement(t, root, repairCacheLocators(ctx, t, store, maildir), original, originalPath, raw)
	runRepairEngine(ctx, t, engine, 0)
}

func TestUnstableSourceUIDUsesContentManifestIdentity(t *testing.T) {
	msg := &plugins.Message{UID: "sequence:1"}
	first := backupMessageUID(msg, "first-hash")
	second := backupMessageUID(msg, "second-hash")
	if first == second {
		t.Fatal("reused POP sequence number overwrites manifest identity")
	}
	msg.UIDScope = "stable-account"
	if backupMessageUID(msg, "first-hash") != msg.UID {
		t.Fatal("stable UID lost")
	}
}

func TestRepairIgnoresLegacyReceiptWithoutDestinationIdentity(t *testing.T) {
	fixture := newEngineFixture(t)
	fixture.source.messages = nil
	if err := fixture.store.MarkDelivered(t.Context(), "first", "mid:legacy", "account", "INBOX", "old"); err != nil {
		t.Fatal(err)
	}
	engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry, WithRepair(true))
	if err != nil {
		t.Fatal(err)
	}
	root := physicalTempDir(t)
	reader, err := target.NewMaildirTarget(map[string]any{"path": root})
	if err != nil {
		t.Fatal(err)
	}
	mustAssert[interface{ SetName(name string) }](t, reader).SetName("first")
	failures := engine.validateRepairedBackups(t.Context(), fixture.source, reader)
	if len(failures) != 0 {
		t.Fatalf("unscoped receipt was treated as current destination evidence: %v", failures)
	}
}
