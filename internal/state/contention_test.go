package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func contentionStore(t *testing.T) (*Store, *sql.Conn) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.UpsertSource(context.Background(), "source", "fixture", "", ""); err != nil {
		t.Fatal(err)
	}
	dsn, err := stateDSN(path, false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") })
	return s, conn
}

func TestTransientWriterContention(t *testing.T) {
	s, holder := contentionStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.UpsertFolderState(ctx, "source", "INBOX", &FolderState{UIDValidity: "1", UIDNext: "3"})
	}()
	select {
	case err := <-done:
		t.Fatalf("write returned before lock release: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := holder.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	fs, err := s.GetFolderState(ctx, "source", "INBOX")
	if err != nil || fs == nil || fs.UIDNext != "3" {
		t.Fatalf("folder = %+v, %v", fs, err)
	}
}

func TestPersistentWriterContentionIsBounded(t *testing.T) {
	s, _ := contentionStore(t)
	start := time.Now()
	err := s.UpsertFolderState(context.Background(), "source", "INBOX", &FolderState{UIDValidity: "1"})
	elapsed := time.Since(start)
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code() != 5 {
		t.Fatalf("want SQLITE_BUSY, got %v", err)
	}
	if elapsed < 4*time.Second || elapsed > 8*time.Second {
		t.Fatalf("busy timeout took %v", elapsed)
	}
	fs, err := s.GetFolderState(context.Background(), "source", "INBOX")
	if err != nil || fs != nil {
		t.Fatalf("failed write persisted: %+v, %v", fs, err)
	}
}

func TestSerializedPoolWaitCanCancel(t *testing.T) {
	s, holder := contentionStore(t)
	// Release the external writer; this test isolates the pool queue.
	if _, err := holder.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = s.UpsertFolderState(ctx, "source", "INBOX", &FolderState{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pool cancellation = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("pool cancellation was delayed")
	}
}

func TestBusyWriterCanCancel(t *testing.T) {
	s, _ := contentionStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.UpsertFolderState(ctx, "source", "INBOX", &FolderState{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy cancellation = %v", err)
	}
	// The driver may finish its native busy handler before observing cancellation.
	if time.Since(start) > 8*time.Second {
		t.Fatal("busy cancellation exceeded timeout bound")
	}
}
