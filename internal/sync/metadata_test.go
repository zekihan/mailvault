package sync

import (
	"context"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMetadataRefreshPersistsKeywordsAndHonorsDryRun(t *testing.T) {
	for _, dry := range []bool{false, true} {
		f := newEngineFixture(t)
		engine, err := NewSyncEngine(f.cfg, f.store, f.registry, WithDryRun(dry))
		if err != nil {
			t.Fatal(err)
		}
		msg := &plugins.Message{UIDScope: "scope", Folder: "INBOX", UID: "1", Flags: []string{"\\Seen", "custom"}}
		entry := plugins.BackupMessage{Source: f.source.Name(), Target: f.targets["first"].Name(), Scope: "scope", Folder: "INBOX", UID: "1", Locator: "locator", RawSHA256: "hash", Flags: []string{"\\Flagged"}}
		if err := f.store.RecordBackupMessage(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
		if err := engine.refreshMetadata(context.Background(), f.source, f.targets["first"], msg); err != nil {
			t.Fatal(err)
		}
		saved, found, err := f.store.FindBackupMessage(context.Background(), entry.Source, entry.Target, entry.Scope, entry.Folder, entry.UID)
		if err != nil || !found {
			t.Fatalf("manifest: %v", err)
		}
		if dry {
			if len(saved.Flags) != 1 || saved.Flags[0] != "\\Flagged" {
				t.Fatal("dry run changed flags")
			}
		} else if len(saved.Flags) != 2 || saved.Flags[1] != "custom" {
			t.Fatalf("flags: %v", saved.Flags)
		}
	}
}
