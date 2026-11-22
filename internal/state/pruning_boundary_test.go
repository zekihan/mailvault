package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPruningPreservesBoundaryAndDeliveryReceipts(t *testing.T) {
	ctx := context.Background()
	s, err := NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertSource(ctx, "source", "imap", "example.invalid", "user"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	for _, uid := range []string{"old", "boundary", "new"} {
		if err := s.MarkMessageFetched(ctx, "source", "INBOX", uid, uid, uid, 10, ""); err != nil {
			t.Fatal(err)
		}
	}
	for uid, stamp := range map[string]string{"old": "2025-01-09 11:59:59", "boundary": "2025-01-09 12:00:00", "new": "2025-01-09 12:00:01"} {
		if _, err := s.db.ExecContext(ctx, `UPDATE messages SET fetched_at = ? WHERE uid = ?`, stamp, uid); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkDelivered(ctx, "target", "identity", "source", "INBOX", "old"); err != nil {
		t.Fatal(err)
	}
	n, err := s.cleanOldMessages(ctx, now, 24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("pruned = %d, %v", n, err)
	}
	for uid, want := range map[string]bool{"old": false, "boundary": true, "new": true} {
		got, err := s.MessageExists(ctx, "source", "INBOX", uid)
		if err != nil || got != want {
			t.Fatalf("%s exists = %v, %v", uid, got, err)
		}
	}
	got, err := s.DeliveryExists(ctx, "target", "identity")
	if err != nil || !got {
		t.Fatalf("receipt exists = %v, %v", got, err)
	}
}
