package state

import (
	"path/filepath"
	"testing"
	"time"
)

func seedPruningMessages(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	for _, uid := range []string{"old", "boundary", "new"} {
		if err := store.MarkMessageFetched(ctx, "source", "INBOX", uid, uid, uid, 10, ""); err != nil {
			t.Fatal(err)
		}
	}
	stampPruningMessages(t, store)
}

func stampPruningMessages(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	for uid, stamp := range map[string]string{"old": "2025-01-09 11:59:59", "boundary": "2025-01-09 12:00:00", "new": "2025-01-09 12:00:01"} {
		if _, err := store.db.ExecContext(ctx, `UPDATE messages SET fetched_at = ? WHERE uid = ?`, stamp, uid); err != nil {
			t.Fatal(err)
		}
	}
}

func pruneAndCheckBoundary(t *testing.T, store *Store) {
	t.Helper()
	now := time.Date(2025, time.January, 10, 12, 0, 0, 0, time.UTC)
	count, err := store.cleanOldMessages(t.Context(), now, 24*time.Hour)
	if err != nil || count != 1 {
		t.Fatalf("pruned = %d, %v", count, err)
	}
	checkPrunedBoundary(t, store)
}

func checkPrunedBoundary(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	for uid, want := range map[string]bool{"old": false, "boundary": true, "new": true} {
		got, err := store.MessageExists(ctx, "source", "INBOX", uid)
		if err != nil || got != want {
			t.Fatalf("%s exists = %v, %v", uid, got, err)
		}
	}
	got, err := store.DeliveryExists(ctx, "target", "identity")
	if err != nil || !got {
		t.Fatalf("receipt exists = %v, %v", got, err)
	}
}

func TestPruningPreservesBoundaryAndDeliveryReceipts(t *testing.T) {
	ctx := t.Context()
	store, err := NewStore(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertSource(ctx, "source", "imap", "example.invalid", "user"); err != nil {
		t.Fatal(err)
	}
	seedPruningMessages(t, store)
	if err := store.MarkDelivered(ctx, "target", "identity", "source", "INBOX", "old"); err != nil {
		t.Fatal(err)
	}
	pruneAndCheckBoundary(t, store)
}
