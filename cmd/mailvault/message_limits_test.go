package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/restore"
	"github.com/zekihan/mailvault/internal/state"
	mailvaultsync "github.com/zekihan/mailvault/internal/sync"
)

func atLimitConfig(t *testing.T, kind string, raw []byte) *config.Config {
	t.Helper()
	return &config.Config{
		MaxMessageBytes: int64(len(raw)),
		Sources:         []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"INBOX"}}},
		Targets:         []config.TargetConfig{{Name: "archive", Type: kind, Config: map[string]any{"path": filepath.Join(physicalTempDir(t), "backup"), "max_listing_entries": 100, "max_listing_locator_bytes": 4096}}},
		Pairs:           []config.PairConfig{{Source: "account", Targets: []string{"archive"}}},
	}
}

func atLimitRegistry(t *testing.T, raw []byte, source *mboxRepairSource) *plugins.Registry {
	t.Helper()
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(values map[string]any) (plugins.Source, error) {
		if values["max_message_bytes"] != int64(len(raw)) {
			t.Fatalf("source limit: %v", values)
		}
		return source, nil
	})
	registry.RegisterTarget("maildir", target.NewMaildirTarget)
	registry.RegisterTarget("mbox", target.NewMboxTarget)
	return registry
}

func syncAtLimitMessage(ctx context.Context, t *testing.T, cfg *config.Config, registry *plugins.Registry, store *state.Store) {
	t.Helper()
	engine, err := mailvaultsync.NewSyncEngine(cfg, store, registry)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := engine.Run(ctx)
	if err != nil || stats.TotalErrors != 0 || stats.TotalWritten != 1 {
		t.Fatalf("sync: %+v, %v", stats, err)
	}
}

func verifyAndRestoreAtLimit(ctx context.Context, t *testing.T, cfg *config.Config, registry *plugins.Registry, kind string, raw []byte, store *state.Store) {
	t.Helper()
	backup, err := registry.CreateTarget(kind, config.TargetPluginConfig(cfg.Targets[0], cfg.MaxMessageBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	reader := mustAssert[plugins.TargetBackupReader](t, backup)
	key := plugins.TargetStateKey(backup)
	report, err := verifyTargetWithStateTarget(ctx, "archive", key, reader)
	if err != nil || report.Verified != 1 || report.Corrupt != 0 || report.Missing != 0 || report.Unverifiable != 0 {
		t.Fatalf("verify: %+v, %v", report, err)
	}
	manifest, err := store.BackupMessages(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	merged := restoreTargetReader{TargetBackupReader: reader, target: key, manifest: manifest}
	appender := &recordingMailbox{}
	result, err := restore.Run(ctx, merged, appender, store, restore.Options{BackupTarget: key, DestinationSource: "destination", DestinationFolder: "Restored"})
	if err != nil || result.Restored != 1 || len(appender.appended) != 1 || !bytes.Equal(appender.appended[0].Raw, raw) {
		t.Fatalf("restore: %+v, %v", result, err)
	}
}

func TestAtLimitSyncMessagesVerifyAndRestore(t *testing.T) {
	for _, kind := range []string{"maildir", "mbox"} {
		t.Run(kind, func(t *testing.T) {
			runAtLimitBackup(t, kind)
		})
	}
}

func runAtLimitBackup(t *testing.T, kind string) {
	t.Helper()
	configureCLI(t)
	ctx := t.Context()
	raw := []byte("Subject: at limit\r\n\r\nFrom escaped body\r\n")
	cfg := atLimitConfig(t, kind, raw)
	source := &mboxRepairSource{message: &plugins.Message{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: raw}}
	registry := atLimitRegistry(t, raw, source)
	store, err := state.NewStore(t.Context(), filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	syncAtLimitMessage(ctx, t, cfg, registry, store)
	verifyAndRestoreAtLimit(ctx, t, cfg, registry, kind, raw, store)
}
