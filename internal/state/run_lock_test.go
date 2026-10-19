package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSyncLockContendsAcrossStoresAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	release, err := first.LockSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := second.LockSync(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second store bypassed lock: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatalf("release is not idempotent: %v", err)
	}
	again, err := second.LockSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := again(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".sync.lock"); err != nil {
		t.Fatal("lock inode was removed after release")
	}
}

func TestSyncLockMemoryStoreSerializesWithoutFile(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	release, err := store.LockSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := store.LockSync(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("private memory store bypassed gate: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	again, err := store.LockSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := again(); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlySyncLockDoesNotCreateSidecar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := NewReadOnlyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if _, err := readOnly.LockSync(context.Background()); !errors.Is(err, errReadOnlySyncLock) {
		t.Fatalf("read-only lock accepted: %v", err)
	}
	if _, err := os.Stat(path + ".sync.lock"); !os.IsNotExist(err) {
		t.Fatal("read-only lock acquisition mutated filesystem")
	}
}

func TestSyncLockUsesOpenedDatabaseAfterDirectoryChange(t *testing.T) {
	original := t.TempDir()
	t.Chdir(original)
	store, err := NewStore("state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	t.Chdir(t.TempDir())
	release, err := store.LockSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(original, "state.db.sync.lock")); err != nil {
		t.Fatalf("lock did not follow opened database: %v", err)
	}
}

func TestSyncLockRecognizesSymlinkAlias(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	alias := filepath.Join(root, "alias.db")
	first, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := os.Symlink(path, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	second, err := NewStore(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	release, err := first.LockSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := second.LockSync(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("alias bypassed database lock: %v", err)
	}
}
