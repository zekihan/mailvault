package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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

func TestLegacyBackupCandidatesRetainUnknownAndExcludeManifestedDeliveries(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertSource(ctx, "src", "imap", "host", "user"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkMessageFetched(ctx, "src", "INBOX", "1", "<id>", "hash-one", 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDelivered(ctx, "target", "hash:hash-one", "src", "INBOX", "1"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBackupMessage(ctx, plugins.BackupMessage{Source: "src", Target: "target", Scope: "scope", Folder: "INBOX", UID: "1", Locator: "object-1", RawSHA256: "hash-one"}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDelivered(ctx, "target", "mid:unknown", "src", "INBOX", "2"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE messages SET fetched_at=datetime('now','-48 hours')"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CleanOldMessages(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.LegacyBackupCandidates(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].UID != "2" || candidates[0].RawSHA256 != "" {
		t.Fatalf("legacy candidates = %+v; want only the unmatched delivery with no hash", candidates)
	}
}

func TestLegacyCandidatesRecognizeContentKeyedPOPManifest(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.MarkDelivered(ctx, "target", "hash:original-hash", "pop", "INBOX", "sequence:1"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBackupMessage(ctx, plugins.BackupMessage{Source: "pop", Target: "target", Folder: "INBOX", UID: "content:original-hash", RawSHA256: "original-hash", Locator: "new/message"}); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.LegacyBackupCandidates(ctx, "target")
	if err != nil || len(candidates) != 0 {
		t.Fatalf("content manifest treated as legacy: %+v %v", candidates, err)
	}
}
