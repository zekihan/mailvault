package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestCorruptDatabaseIsRejectedAndPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	original := []byte("fixture database with an invalid SQLite header")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		open func(string) (*Store, error)
	}{
		{"writable", NewStore},
		{"read-only", NewReadOnlyStore},
		{"cleanup", func(path string) (*Store, error) { return NewCleanupStore(t.Context(), path) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := tc.open(path)
			if store != nil {
				_ = store.Close()
				t.Fatal("opened corrupt database")
			}
			var dbErr *sqlite.Error
			if !errors.As(err, &dbErr) || dbErr.Code() != sqlite3.SQLITE_NOTADB {
				t.Fatalf("corrupt database error = %v, want SQLITE_NOTADB", err)
			}
			if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, original) {
				t.Fatalf("open altered the corrupt database: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("open left unexpected files: %v, %v", entries, err)
			}
		})
	}
}

func TestConcurrentDatabaseWritersPersistAllReceipts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var writers [2]*Store
	for i := range writers {
		var err error
		writers[i], err = NewStore(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = writers[i].Close() })
	}

	const count = 24
	start := make(chan struct{})
	results := make(chan error, count)
	for i := range count {
		go func() {
			<-start
			uid := fmt.Sprintf("%02d", i)
			writer := writers[i%len(writers)]
			err := writer.RecordBackupMessage(ctx, plugins.BackupMessage{
				Source: "account", Target: "archive", Scope: "generation", Folder: "INBOX",
				UID: uid, Locator: "new/" + uid, RawSHA256: fmt.Sprintf("%064x", i+1),
			})
			if err == nil {
				err = writer.MarkDelivered(ctx, "archive", "fixture:"+uid, "account", "INBOX", uid)
			}
			results <- err
		}()
	}
	close(start)
	for range count {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	for _, writer := range writers {
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}

	readOnly, err := NewReadOnlyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	entries, err := readOnly.BackupMessages(ctx, "archive")
	if err != nil || len(entries) != count {
		t.Fatalf("manifest count = %d, error = %v, want %d", len(entries), err, count)
	}
	for i, entry := range entries {
		uid := fmt.Sprintf("%02d", i)
		if entry.UID != uid || entry.Locator != "new/"+uid || entry.RawSHA256 != fmt.Sprintf("%064x", i+1) {
			t.Fatalf("unexpected manifest entry: %+v", entry)
		}
		delivered, err := readOnly.DeliveryExists(ctx, "archive", "fixture:"+uid)
		if err != nil || !delivered {
			t.Fatalf("missing delivery receipt for UID %s: %v", uid, err)
		}
	}
}

func TestRestoreJobMigratesLegacySchemaAndPinsNewGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.db.ExecContext(ctx, `CREATE TABLE restore_jobs (
		destination_source TEXT NOT NULL, destination_folder TEXT NOT NULL, backup_target TEXT NOT NULL,
		status TEXT NOT NULL, started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(destination_source,destination_folder,backup_target)
	);
	INSERT INTO restore_jobs(destination_source,destination_folder,backup_target,status)
	VALUES ('account','old-folder','archive','active'), ('account','interrupted','archive','preparing');`); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.BeginRestoreJob(ctx, "account", "old-folder", "archive"); !errors.Is(err, ErrRestoreJobIdentityUnavailable) {
		t.Fatalf("legacy active job should require a generation: %v", err)
	}
	if _, _, err := store.BeginRestoreJob(ctx, "account", "interrupted", "archive"); !errors.Is(err, ErrRestoreJobUncertain) {
		t.Fatalf("legacy preparing job should require explicit resolution: %v", err)
	}
	if err := store.ResolveRestoreJobSetup(ctx, "account", "interrupted", "archive", true); err != nil {
		t.Fatal(err)
	}
	resuming, generation, err := store.BeginRestoreJob(ctx, "account", "interrupted", "archive")
	if err != nil || !resuming || generation != "" {
		t.Fatalf("resolved job: resuming = %t, generation = %q, error = %v", resuming, generation, err)
	}
	if err := store.ActivateRestoreJob(ctx, "account", "interrupted", "archive", "42"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resuming, generation, err = reopened.BeginRestoreJob(ctx, "account", "interrupted", "archive")
	if err != nil || !resuming || generation != "42" {
		t.Fatalf("reopened job: resuming = %t, generation = %q, error = %v", resuming, generation, err)
	}
	if _, _, err := reopened.BeginRestoreJob(ctx, "account", "old-folder", "archive"); !errors.Is(err, ErrRestoreJobIdentityUnavailable) {
		t.Fatalf("migration changed an unrelated restore job: %v", err)
	}
}
