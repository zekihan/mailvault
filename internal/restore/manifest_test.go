package restore

import (
	"context"
	"errors"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMergeBackupMessagesDeduplicatesManifestAliases(t *testing.T) {
	physical := []plugins.BackupMessage{{Target: "archive", Locator: "objects/one"}}
	manifest := []plugins.BackupMessage{
		{Source: "z-source", Target: "archive", Scope: "z-scope", Folder: "Inbox", UID: "9", Locator: "objects/one", RawSHA256: "abcd"},
		{Source: "a-source", Target: "archive", Scope: "a-scope", Folder: "Sent", UID: "2", Locator: "objects/one", RawSHA256: "abcd"},
	}
	merged, err := MergeBackupMessages(physical, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 1 || merged[0].Source != "a-source" || merged[0].Folder != "Sent" {
		t.Fatalf("aliases were not deterministically collapsed: %+v", merged)
	}
}

func TestMergeBackupMessagesRejectsMissingManifestObject(t *testing.T) {
	_, err := MergeBackupMessages(nil, []plugins.BackupMessage{{Locator: "missing"}})
	if !errors.Is(err, plugins.ErrBackupMessageMissing) {
		t.Fatalf("error = %v, want missing backup object", err)
	}
}

func TestMergeBackupMessagesRejectsConflictingAliasHashes(t *testing.T) {
	physical := []plugins.BackupMessage{{Locator: "object"}}
	manifest := []plugins.BackupMessage{{Locator: "object", RawSHA256: "one"}, {Locator: "object", RawSHA256: "two"}}
	_, err := MergeBackupMessages(physical, manifest)
	if !errors.Is(err, plugins.ErrBackupMessageCorrupt) {
		t.Fatalf("error = %v, want corrupt manifest", err)
	}
}

func TestLegacyBackupRequiresExplicitOptIn(t *testing.T) {
	reader := fixtureReader{entries: []plugins.BackupMessage{{Locator: "legacy"}}, message: &plugins.Message{Raw: []byte("content")}}
	appender := &fixtureAppender{}
	result, err := Run(context.Background(), reader, appender, nil, Options{BackupTarget: "archive", DestinationSource: "account", DestinationFolder: "restore", DryRun: true})
	if err != nil || result.Unverified != 1 || result.Planned != 1 {
		t.Fatalf("dry-run legacy result=%+v err=%v", result, err)
	}
	_, err = Run(context.Background(), reader, appender, newFixtureReceipts(), Options{BackupTarget: "archive", DestinationSource: "account", DestinationFolder: "restore"})
	if err == nil || appender.ensured != 0 {
		t.Fatalf("legacy restore proceeded without opt-in: err=%v appends=%d", err, appender.appended)
	}
}
