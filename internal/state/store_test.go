package state

import (
	"context"
	"testing"
	"time"
)

func TestStore_Basic(t *testing.T) {
	ctx := context.Background()

	// Create temp database
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore() = %v, want nil", err)
	}
	defer store.Close()

	// Test UpsertSource
	if err := store.UpsertSource(ctx, "gmail", "imap", "imap.gmail.com", "user@gmail.com"); err != nil {
		t.Fatalf("UpsertSource() = %v, want nil", err)
	}

	// Test folder state
	fs := &FolderState{
		UIDValidity: "12345",
		UIDNext:     "100",
		ModSeq:      "5000",
	}
	if err := store.UpsertFolderState(ctx, "gmail", "INBOX", fs); err != nil {
		t.Fatalf("UpsertFolderState() = %v, want nil", err)
	}

	// Retrieve folder state
	got, err := store.GetFolderState(ctx, "gmail", "INBOX")
	if err != nil {
		t.Fatalf("GetFolderState() = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("GetFolderState() = nil, want FolderState")
	}
	if got.UIDValidity != "12345" || got.UIDNext != "100" || got.ModSeq != "5000" {
		t.Fatalf("GetFolderState() = %+v, want UIDValidity=12345, UIDNext=100, ModSeq=5000", got)
	}

	// Test message tracking
	if err := store.MarkMessageFetched(ctx, "gmail", "INBOX", "1", "<msg1@example.com>", "hash1", 1024, "2024-01-01T00:00:00Z"); err != nil {
		t.Fatalf("MarkMessageFetched() = %v, want nil", err)
	}

	// Check exists by UID
	exists, err := store.MessageExists(ctx, "gmail", "INBOX", "1")
	if err != nil {
		t.Fatalf("MessageExists() = %v, want nil", err)
	}
	if !exists {
		t.Fatal("MessageExists() = false, want true")
	}

	// Check not exists
	exists, err = store.MessageExists(ctx, "gmail", "INBOX", "999")
	if err != nil {
		t.Fatalf("MessageExists() = %v, want nil", err)
	}
	if exists {
		t.Fatal("MessageExists() = true, want false")
	}

	// Test deduplication by Message-ID
	found, err := store.FindByMessageID(ctx, "<msg1@example.com>")
	if err != nil {
		t.Fatalf("FindByMessageID() = %v, want nil", err)
	}
	if len(found) != 1 || found[0].UID != "1" {
		t.Fatalf("FindByMessageID() = %+v, want 1 record with UID=1", found)
	}

	// Test deduplication by Content-Hash
	found, err = store.FindByContentHash(ctx, "hash1")
	if err != nil {
		t.Fatalf("FindByContentHash() = %v, want nil", err)
	}
	if len(found) != 1 || found[0].UID != "1" {
		t.Fatalf("FindByContentHash() = %+v, want 1 record with UID=1", found)
	}

	// Test OAuth tokens
	tok := &OAuthToken{
		AccessToken:  "access-123",
		RefreshToken: "refresh-123",
		Expiry:       time.Now().Add(time.Hour),
	}
	if err := store.UpsertOAuthToken(ctx, "gmail", tok); err != nil {
		t.Fatalf("UpsertOAuthToken() = %v, want nil", err)
	}

	gotTok, err := store.GetOAuthToken(ctx, "gmail")
	if err != nil {
		t.Fatalf("GetOAuthToken() = %v, want nil", err)
	}
	if gotTok == nil || gotTok.AccessToken != "access-123" {
		t.Fatalf("GetOAuthToken() = %+v, want access-123", gotTok)
	}

	// Test stats
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() = %v, want nil", err)
	}
	if stats["sources"] != 1 || stats["folders"] != 1 || stats["messages"] != 1 || stats["oauth"] != 1 {
		t.Fatalf("Stats() = %+v, want sources=1, folders=1, messages=1, oauth=1", stats)
	}
}

func TestStore_CleanOldMessages(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("UTC+3", 3*60*60)
	t.Cleanup(func() { time.Local = local })

	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore() = %v, want nil", err)
	}
	defer store.Close()

	// Insert a recent message.
	if err := store.UpsertSource(ctx, "src", "imap", "host", "user"); err != nil {
		t.Fatalf("UpsertSource() = %v", err)
	}
	if err := store.MarkMessageFetched(ctx, "src", "INBOX", "1", "<msg1>", "hash1", 100, "2024-01-01T00:00:00Z"); err != nil {
		t.Fatalf("MarkMessageFetched() = %v", err)
	}

	// Clean messages older than 1 hour (should not delete recent)
	deleted, err := store.CleanOldMessages(ctx, time.Hour)
	if err != nil {
		t.Fatalf("CleanOldMessages() = %v", err)
	}
	if deleted != 0 {
		t.Fatalf("CleanOldMessages() = %d, want 0", deleted)
	}

	if err := store.MarkMessageFetched(ctx, "src", "INBOX", "2", "<msg2>", "hash2", 100, "2024-01-01T00:00:00Z"); err != nil {
		t.Fatalf("MarkMessageFetched() = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE messages SET fetched_at = datetime('now', '-2 hours') WHERE uid = ?", "2"); err != nil {
		t.Fatalf("set old fetched_at: %v", err)
	}

	deleted, err = store.CleanOldMessages(ctx, time.Hour)
	if err != nil {
		t.Fatalf("CleanOldMessages() = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("CleanOldMessages() = %d, want 1", deleted)
	}
	for uid, want := range map[string]bool{"1": true, "2": false} {
		exists, err := store.MessageExists(ctx, "src", "INBOX", uid)
		if err != nil {
			t.Fatalf("MessageExists(%q) = %v", uid, err)
		}
		if exists != want {
			t.Errorf("MessageExists(%q) = %v, want %v", uid, exists, want)
		}
	}
}
