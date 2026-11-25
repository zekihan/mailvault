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

func seedCleanupState(ctx context.Context, t *testing.T) {
	t.Helper()
	store, err := state.NewStore(ctx, filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
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
}

func ageCleanupMetadata(ctx context.Context, t *testing.T) {
	t.Helper()
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
}

func runCleanupCommand(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newStateCleanupCommand()
	cmd.SetArgs(args)
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func assertCleanupReceiptsRetained(ctx context.Context, t *testing.T) {
	t.Helper()
	readOnly, err := state.NewReadOnlyStore(ctx, filepath.Join(stateDir, "state.db"))
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

func TestStateCleanupPreviewsAndRetainsReceipts(t *testing.T) {
	previousStateDir := stateDir
	defer func() { stateDir = previousStateDir }()
	stateDir = physicalTempDir(t)
	ctx := t.Context()
	seedCleanupState(ctx, t)
	ageCleanupMetadata(ctx, t)
	path := filepath.Join(stateDir, "state.db")
	before := readCommandFixture(t, path)
	previewOut := runCleanupCommand(t, "--older-than", "24h")
	if !strings.Contains(previewOut, "1 old message metadata") || !strings.Contains(previewOut, "use --apply") {
		t.Fatalf("preview output = %q", previewOut)
	}
	if !bytes.Equal(before, readCommandFixture(t, path)) {
		t.Fatal("cleanup preview changed the state database")
	}
	applyOut := runCleanupCommand(t, "--older-than", "24h", "--apply")
	if !strings.Contains(applyOut, "Removed 1") {
		t.Fatalf("apply output = %q", applyOut)
	}
	assertCleanupReceiptsRetained(ctx, t)
}
