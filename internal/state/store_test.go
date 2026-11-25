package state

import (
	"testing"
	"time"
)

func openBasicStore(t *testing.T) *Store {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
	store, err := NewStore(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("NewStore(t.Context(), ) = %v, want nil", err)
	}
	return store
}

func checkBasicFolderState(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	if err := store.UpsertSource(ctx, "gmail", "imap", "imap.gmail.com", "user@gmail.com"); err != nil {
		t.Fatalf("UpsertSource() = %v, want nil", err)
	}
	fs := &FolderState{
		UIDValidity: "12345",
		UIDNext:     "100",
		ModSeq:      "5000",
	}
	if err := store.UpsertFolderState(ctx, "gmail", "INBOX", fs); err != nil {
		t.Fatalf("UpsertFolderState() = %v, want nil", err)
	}
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
}

func checkBasicMessageTracking(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	if err := store.MarkMessageFetched(ctx, "gmail", "INBOX", "1", "<msg1@example.com>", "hash1", 1024, "2024-01-01T00:00:00Z"); err != nil {
		t.Fatalf("MarkMessageFetched() = %v, want nil", err)
	}
	checkBasicMessageExists(t, store)
	checkBasicMessageLookup(t, store)
}

func checkBasicMessageExists(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	exists, err := store.MessageExists(ctx, "gmail", "INBOX", "1")
	if err != nil {
		t.Fatalf("MessageExists() = %v, want nil", err)
	}
	if !exists {
		t.Fatal("MessageExists() = false, want true")
	}
	exists, err = store.MessageExists(ctx, "gmail", "INBOX", "999")
	if err != nil {
		t.Fatalf("MessageExists() = %v, want nil", err)
	}
	if exists {
		t.Fatal("MessageExists() = true, want false")
	}
}

func checkBasicMessageLookup(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	found, err := store.FindByMessageID(ctx, "<msg1@example.com>")
	if err != nil {
		t.Fatalf("FindByMessageID() = %v, want nil", err)
	}
	if len(found) != 1 || found[0].UID != "1" {
		t.Fatalf("FindByMessageID() = %+v, want 1 record with UID=1", found)
	}
	found, err = store.FindByContentHash(ctx, "hash1")
	if err != nil {
		t.Fatalf("FindByContentHash() = %v, want nil", err)
	}
	if len(found) != 1 || found[0].UID != "1" {
		t.Fatalf("FindByContentHash() = %+v, want 1 record with UID=1", found)
	}
}

func checkBasicTokenAndStats(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
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
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() = %v, want nil", err)
	}
	if stats["sources"] != 1 || stats["folders"] != 1 || stats["messages"] != 1 || stats["oauth"] != 1 {
		t.Fatalf("Stats() = %+v, want sources=1, folders=1, messages=1, oauth=1", stats)
	}
}

func TestStore_Basic(t *testing.T) {
	store := openBasicStore(t)
	defer store.Close()
	checkBasicFolderState(t, store)
	checkBasicMessageTracking(t, store)
	checkBasicTokenAndStats(t, store)
}

func TestStore_CleanOldMessages(t *testing.T) {
	local := time.Local //nolint:gosmopolitan // test zone fixture; local handling is the scenario
	zone := time.FixedZone("UTC+3", 3*60*60)
	// A timer can read the process time zone while the fixture sets up its clock.
	readLocal := make(chan *time.Location, 1)
	time.AfterFunc(0, func() { readLocal <- time.Now().Location() })
	now := time.Date(2024, time.January, 2, 0, 30, 0, 0, zone)
	if got := <-readLocal; got != local {
		t.Fatal("setting up the cleanup clock changed the process time zone")
	}

	ctx := t.Context()
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	store, err := NewStore(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("NewStore(t.Context(), ) = %v, want nil", err)
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
	ctx := t.Context()
	store, err := NewStore(t.Context(), t.TempDir()+"/state.db")
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
