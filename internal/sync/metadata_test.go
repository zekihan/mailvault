package sync

import (
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMetadataRefreshPersistsKeywordsAndHonorsDryRun(t *testing.T) {
	for _, dry := range []bool{false, true} {
		fixture := newEngineFixture(t)
		engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry, WithDryRun(dry))
		if err != nil {
			t.Fatal(err)
		}
		msg := &plugins.Message{UIDScope: "scope", Folder: "INBOX", UID: "1", Flags: []string{"\\Seen", "custom"}}
		entry := plugins.BackupMessage{Source: fixture.source.Name(), Target: fixture.targets["first"].Name(), Scope: "scope", Folder: "INBOX", UID: "1", Locator: "locator", RawSHA256: "hash", Flags: []string{"\\Flagged"}}
		if err := fixture.store.RecordBackupMessage(t.Context(), &entry); err != nil {
			t.Fatal(err)
		}
		if err := engine.refreshMetadata(t.Context(), fixture.source, fixture.targets["first"], msg); err != nil {
			t.Fatal(err)
		}
		saved, found, err := fixture.store.FindBackupMessage(t.Context(), entry.Source, entry.Target, entry.Scope, entry.Folder, entry.UID)
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
