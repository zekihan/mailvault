package main

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/state"
)

func TestStateCleanupPreviewsAndRetainsReceipts(t *testing.T) {
	previousStateDir := stateDir
	defer func() { stateDir = previousStateDir }()
	stateDir = t.TempDir()
	store, err := state.NewStore(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Age the metadata using SQLite so the test does not depend on the clock.
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE messages SET fetched_at = datetime('now', '-48 hours') WHERE uid = '1'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	preview := newStateCleanupCommand()
	preview.SetArgs([]string{"--older-than", "24h"})
	var output bytes.Buffer
	preview.SetOut(&output)
	if err := preview.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "1 old message metadata") || !strings.Contains(output.String(), "use --apply") {
		t.Fatalf("preview output = %q", output.String())
	}

	apply := newStateCleanupCommand()
	apply.SetArgs([]string{"--older-than", "24h", "--apply"})
	output.Reset()
	apply.SetOut(&output)
	if err := apply.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Removed 1") {
		t.Fatalf("apply output = %q", output.String())
	}

	readOnly, err := state.NewReadOnlyStore(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if count, err := readOnly.CountOldMessages(ctx, 24*time.Hour); err != nil || count != 0 {
		t.Fatalf("old message metadata after cleanup = %d, %v; want 0, nil", count, err)
	}
	if exists, err := readOnly.DeliveryExists(ctx, "target", "hash"); err != nil || !exists {
		t.Fatalf("delivery receipt after cleanup = %t, %v; want true, nil", exists, err)
	}
	known, err := readOnly.KnownUIDs(ctx, "src", "target", "scope", "INBOX")
	if err != nil || !known["1"] {
		t.Fatalf("incremental receipt after cleanup = %v, %v; want UID 1, nil", known, err)
	}
}
