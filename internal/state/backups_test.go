package state

import (
	"context"
	"path/filepath"
	"strings"
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

func TestBackupMessagesByHashFiltersOrdersAndUsesIndex(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, item := range []plugins.BackupMessage{
		{Source: "z", Target: "archive", Scope: "b", Folder: "INBOX", UID: "2", Locator: "last", RawSHA256: "wanted"},
		{Source: "a", Target: "archive", Scope: "b", Folder: "Sent", UID: "1", Locator: "first", RawSHA256: "wanted"},
		{Source: "a", Target: "archive", Scope: "a", Folder: "Sent", UID: "9", Locator: "second", RawSHA256: "wanted"},
		{Source: "0", Target: "archive", Scope: "a", Folder: "INBOX", UID: "1", Locator: "unrelated", RawSHA256: "other"},
		{Source: "a", Target: "another-target", Scope: "a", Folder: "INBOX", UID: "1", Locator: "other-target", RawSHA256: "wanted"},
	} {
		if err := store.RecordBackupMessage(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.BackupMessagesByHash(ctx, "archive", "wanted")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"second", "first", "last"}
	if len(got) != len(want) {
		t.Fatalf("matching rows = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Locator != want[i] {
			t.Fatalf("row %d locator = %q, want %q", i, got[i].Locator, want[i])
		}
	}

	rows, err := store.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+backupMessagesByHashQuery, "archive", "wanted")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "SEARCH backup_messages USING INDEX idx_backup_messages_target_hash_order (target_name=? AND raw_sha256=?)") || strings.Contains(joined, "USE TEMP B-TREE") {
		t.Fatalf("query plan does not use ordered composite index: %s", joined)
	}
}

func TestBackupHashIndexIsAddedToExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP INDEX idx_backup_messages_target_hash_order`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var exists bool
	if err := store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='index' AND name='idx_backup_messages_target_hash_order')`).Scan(&exists); err != nil || !exists {
		t.Fatalf("composite index exists=%t err=%v", exists, err)
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
