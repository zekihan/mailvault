package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyStorePreservesDeliveryState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.MarkDelivered(ctx, "target", "message", "account", "INBOX", "1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := NewReadOnlyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	exists, err := ro.DeliveryExists(ctx, "target", "message")
	if err != nil || !exists {
		t.Fatalf("lost read-only delivery: %v %v", exists, err)
	}
	if err := ro.MarkDelivered(ctx, "target", "other", "account", "INBOX", "2"); err == nil {
		t.Fatal("read-only store accepted write")
	}
}
func TestReadOnlyLegacyStoreDoesNotMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("DROP TABLE deliveries"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := NewReadOnlyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	exists, err := ro.DeliveryExists(context.Background(), "target", "message")
	if err != nil || exists {
		t.Fatalf("legacy delivery check: %v %v", exists, err)
	}
	var count int
	if err := ro.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='deliveries'").Scan(&count); err != nil || count != 0 {
		t.Fatal("read-only open migrated legacy schema")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ro.DeliveryExists(ctx, "target", "message"); !errors.Is(err, context.Canceled) {
		t.Fatal("legacy read ignored cancellation")
	}
}
func TestReadOnlyStoreDoesNotCreateMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	store, err := NewReadOnlyStore(path)
	if err == nil {
		_ = store.Close()
		t.Fatal("read-only store created missing file")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing database was created")
	}
}
