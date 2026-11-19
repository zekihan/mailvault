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
	zone := time.FixedZone("UTC+3", 3*60*60)
	// A timer can read the process time zone while the fixture sets up its clock.
	readLocal := make(chan *time.Location, 1)
	time.AfterFunc(0, func() { readLocal <- time.Now().Location() })
	now := time.Date(2024, time.January, 2, 0, 30, 0, 0, zone)
	if got := <-readLocal; got != local {
		t.Fatal("setting up the cleanup clock changed the process time zone")
	}

	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore() = %v, want nil", err)
	}
	defer store.Close()

	if err := store.UpsertSource(ctx, "src", "imap", "host", "user"); err != nil {
		t.Fatalf("UpsertSource() = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO messages (source_name, folder, uid, fetched_at) VALUES
			('src', 'INBOX', '1', '2024-01-01 21:30:00'),
			('src', 'INBOX', '2', '2024-01-01 20:29:59'),
			('src', 'INBOX', '3', '2024-01-01 20:30:00')
	`); err != nil {
		t.Fatalf("insert message metadata: %v", err)
	}

	deleted, err := store.cleanOldMessages(ctx, now, time.Hour)
	if err != nil {
		t.Fatalf("CleanOldMessages() = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("CleanOldMessages() = %d, want 1", deleted)
	}
	for uid, want := range map[string]bool{"1": true, "2": false, "3": true} {
		exists, err := store.MessageExists(ctx, "src", "INBOX", uid)
		if err != nil {
			t.Fatalf("MessageExists(%q) = %v", uid, err)
		}
		if exists != want {
			t.Errorf("MessageExists(%q) = %v, want %v", uid, exists, want)
		}
	}
}

func TestCleanupOldMessageMetadataRetainsDeliveryReceipts(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertSource(ctx, "src", "imap", "host", "user"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkMessageFetched(ctx, "src", "INBOX", "1", "<id>", "hash", 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDelivered(ctx, "target", "hash", "src", "INBOX", "1"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUIDDelivered(ctx, "src", "target", "scope", "INBOX", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE messages SET fetched_at = datetime('now', '-48 hours') WHERE uid = '1'"); err != nil {
		t.Fatal(err)
	}
	count, err := store.CountOldMessages(ctx, 24*time.Hour)
	if err != nil || count != 1 {
		t.Fatalf("CountOldMessages() = %d, %v; want 1, nil", count, err)
	}
	deleted, err := store.CleanOldMessages(ctx, 24*time.Hour)
	if err != nil || deleted != 1 {
		t.Fatalf("CleanOldMessages() = %d, %v; want 1, nil", deleted, err)
	}
	for name, query := range map[string]string{
		"delivery":            `SELECT EXISTS(SELECT 1 FROM deliveries WHERE target_name='target' AND identity='hash')`,
		"incremental receipt": `SELECT EXISTS(SELECT 1 FROM imap_receipts WHERE source='src' AND target='target' AND scope='scope' AND folder='INBOX' AND uid='1')`,
	} {
		var exists bool
		if err := store.db.QueryRowContext(ctx, query).Scan(&exists); err != nil || !exists {
			t.Errorf("%s after cleanup = %t, %v; want true, nil", name, exists, err)
		}
	}
}
