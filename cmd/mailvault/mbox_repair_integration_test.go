package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/restore"
	"github.com/zekihan/mailvault/internal/state"
	mailvaultsync "github.com/zekihan/mailvault/internal/sync"
)

type mboxRepairSource struct{ message *plugins.Message }

func (*mboxRepairSource) Name() string                  { return "account" }
func (*mboxRepairSource) Type() string                  { return "fixture" }
func (*mboxRepairSource) Connect(context.Context) error { return nil }
func (*mboxRepairSource) Close() error                  { return nil }
func (*mboxRepairSource) ListFolders(context.Context) ([]string, error) {
	return []string{"INBOX"}, nil
}
func (s *mboxRepairSource) FetchMessages(context.Context, []string, int64, int64) (<-chan *plugins.Message, <-chan error) {
	messages := make(chan *plugins.Message, 1)
	messages <- s.message
	close(messages)
	errors := make(chan error)
	close(errors)
	return messages, errors
}

func runMboxRepairSync(ctx context.Context, t *testing.T, cfg *config.Config, registry *plugins.Registry, store *state.Store, options ...mailvaultsync.SyncOption) {
	t.Helper()
	engine, err := mailvaultsync.NewSyncEngine(cfg, store, registry, options...)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := engine.Run(ctx)
	if err != nil || stats.TotalErrors != 0 {
		t.Fatalf("sync: %+v %v", stats, err)
	}
}

func truncateMbox(t *testing.T, path string, cut int64) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(0)
	if cut != 0 {
		size = info.Size() - cut
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
}

func assertMboxRepairVerified(ctx context.Context, t *testing.T, path string) (plugins.TargetBackupReader, string) {
	t.Helper()
	backup, err := target.NewMboxTarget(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backup.Close() })
	mustAssert[interface{ SetName(name string) }](t, backup).SetName("archive")
	key := plugins.TargetStateKey(backup)
	reader := mustAssert[plugins.TargetBackupReader](t, backup)
	report, err := verifyTargetWithStateTarget(ctx, "archive", key, reader)
	if err != nil || report.Verified != 1 || report.Corrupt != 0 || report.Missing != 0 || report.Unverifiable != 0 {
		t.Fatalf("verify after repair: %+v %v", report, err)
	}
	return reader, key
}

func assertMboxRepairRestored(ctx context.Context, t *testing.T, reader plugins.TargetBackupReader, key string, store *state.Store, raw []byte) {
	t.Helper()
	manifest, err := store.BackupMessages(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	superseded, err := store.SupersededBackupObjects(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	merged := restoreTargetReader{TargetBackupReader: reader, target: key, manifest: manifest, superseded: superseded}
	appender := &recordingMailbox{}
	result, err := restore.Run(ctx, merged, appender, store, restore.Options{BackupTarget: key, DestinationSource: "destination", DestinationFolder: "Restored"})
	if err != nil || result.Restored != 1 || len(appender.appended) != 1 {
		t.Fatalf("restore after repair: %+v %v", result, err)
	}
	if !bytes.Equal(appender.appended[0].Raw, raw) {
		t.Fatal("restored bytes changed")
	}
}

func runRepairedTruncatedMbox(t *testing.T, cut int64) {
	t.Helper()
	configureCLI(t)
	ctx := t.Context()
	path := filepath.Join(physicalTempDir(t), "archive.mbox")
	store, err := state.NewStore(t.Context(), filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw := []byte("Subject: repair\n\nbody content\n")
	source := &mboxRepairSource{message: &plugins.Message{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: raw}}
	cfg := &config.Config{Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"INBOX"}}}, Targets: []config.TargetConfig{{Name: "archive", Type: "mbox", Config: map[string]any{"path": path}}}, Pairs: []config.PairConfig{{Source: "account", Targets: []string{"archive"}}}}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]any) (plugins.Source, error) { return source, nil })
	registry.RegisterTarget("mbox", target.NewMboxTarget)
	runMboxRepairSync(ctx, t, cfg, registry, store)
	truncateMbox(t, path, cut)
	runMboxRepairSync(ctx, t, cfg, registry, store, mailvaultsync.WithRepair(true))
	reader, key := assertMboxRepairVerified(ctx, t, path)
	assertMboxRepairRestored(ctx, t, reader, key, store, raw)
}

func TestRepairedTruncatedMboxVerifiesAndRestores(t *testing.T) {
	for _, cut := range []int64{0, 2, 5} {
		name := "empty"
		if cut != 0 {
			name = fmt.Sprintf("%d-bytes", cut)
		}
		t.Run(name, func(t *testing.T) {
			runRepairedTruncatedMbox(t, cut)
		})
	}
}
