package state

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestBackupMessageManifestPreservesSourceIdentityAndMetadata(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	want := plugins.BackupMessage{Source: "account", Target: "archive", Scope: "generation", Folder: "Sent", UID: "42", Locator: "new/message", RawSHA256: "abc", Flags: []string{"\\Seen"}, InternalDate: "2026-10-20T12:00:00Z"}
	if err := store.RecordBackupMessage(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.FindBackupMessage(context.Background(), want.Source, want.Target, want.Scope, want.Folder, want.UID)
	if err != nil || !found {
		t.Fatalf("find manifest: found=%t err=%v", found, err)
	}
	if got.Locator != want.Locator || got.RawSHA256 != want.RawSHA256 || got.InternalDate != want.InternalDate || len(got.Flags) != 1 || got.Flags[0] != want.Flags[0] {
		t.Fatalf("manifest = %+v", got)
	}
}
