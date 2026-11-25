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
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		open func(string) (*Store, error)
	}{
		{"writable", func(path string) (*Store, error) { return NewStore(t.Context(), path) }},
		{"read-only", func(path string) (*Store, error) { return NewReadOnlyStore(t.Context(), path) }},
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

func openConcurrentWriters(t *testing.T, path string) [2]*Store {
	t.Helper()
	var writers [2]*Store
	for i := range writers {
		var err error
		writers[i], err = NewStore(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = writers[i].Close() })
	}
	return writers
}

func launchConcurrentReceipts(ctx context.Context, writers [2]*Store) chan error {
	results := make(chan error, 24)
	start := make(chan struct{})
	for i := range 24 {
		go func() {
			<-start
			uid := fmt.Sprintf("%02d", i)
			writer := writers[i%len(writers)]
			err := writer.RecordBackupMessage(ctx, &plugins.BackupMessage{
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
	return results
}

func collectConcurrentReceipts(t *testing.T, results <-chan error) {
	t.Helper()
	for range 24 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}

func checkConcurrentManifest(t *testing.T, entries []plugins.BackupMessage) {
	t.Helper()
	for i := range entries {
		uid := fmt.Sprintf("%02d", i)
		if entries[i].UID != uid || entries[i].Locator != "new/"+uid || entries[i].RawSHA256 != fmt.Sprintf("%064x", i+1) {
			t.Fatalf("unexpected manifest entry: %+v", entries[i])
		}
	}
}

func checkConcurrentReceipts(ctx context.Context, t *testing.T, readOnly *Store, count int) {
	t.Helper()
	for i := range count {
		uid := fmt.Sprintf("%02d", i)
		delivered, err := readOnly.DeliveryExists(ctx, "archive", "fixture:"+uid)
		if err != nil || !delivered {
			t.Fatalf("missing delivery receipt for UID %s: %v", uid, err)
		}
	}
}

func TestConcurrentDatabaseWritersPersistAllReceipts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	writers := openConcurrentWriters(t, path)
	const count = 24
	results := launchConcurrentReceipts(ctx, writers)
	collectConcurrentReceipts(t, results)
	for _, writer := range writers {
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	readOnly, err := NewReadOnlyStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	entries, err := readOnly.BackupMessages(ctx, "archive")
	if err != nil || len(entries) != count {
		t.Fatalf("manifest count = %d, error = %v, want %d", len(entries), err, count)
	}
	checkConcurrentManifest(t, entries)
	checkConcurrentReceipts(ctx, t, readOnly, count)
}

func seedLegacyRestoreJobs(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.ExecContext(t.Context(), `CREATE TABLE restore_jobs (
		destination_source TEXT NOT NULL, destination_folder TEXT NOT NULL, backup_target TEXT NOT NULL,
		status TEXT NOT NULL, started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(destination_source,destination_folder,backup_target)
	);
	INSERT INTO restore_jobs(destination_source,destination_folder,backup_target,status)
	VALUES ('account','old-folder','archive','active'), ('account','interrupted','archive','preparing');`); err != nil {
		t.Fatal(err)
	}
}

func checkLegacyRestoreJobGuards(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	if _, _, err := store.BeginRestoreJob(ctx, "account", "old-folder", "archive"); !errors.Is(err, ErrRestoreJobIdentityUnavailable) {
		t.Fatalf("legacy active job should require a generation: %v", err)
	}
	if _, _, err := store.BeginRestoreJob(ctx, "account", "interrupted", "archive"); !errors.Is(err, ErrRestoreJobUncertain) {
		t.Fatalf("legacy preparing job should require explicit resolution: %v", err)
	}
}

func resolveLegacyRestoreJob(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
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
}

func checkReopenedRestoreJob(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	resuming, generation, err := store.BeginRestoreJob(ctx, "account", "interrupted", "archive")
	if err != nil || !resuming || generation != "42" {
		t.Fatalf("reopened job: resuming = %t, generation = %q, error = %v", resuming, generation, err)
	}
	if _, _, err := store.BeginRestoreJob(ctx, "account", "old-folder", "archive"); !errors.Is(err, ErrRestoreJobIdentityUnavailable) {
		t.Fatalf("migration changed an unrelated restore job: %v", err)
	}
}

func TestRestoreJobMigratesLegacySchemaAndPinsNewGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := NewStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedLegacyRestoreJobs(t, store)
	checkLegacyRestoreJobGuards(t, store)
	resolveLegacyRestoreJob(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	checkReopenedRestoreJob(t, reopened)
}
