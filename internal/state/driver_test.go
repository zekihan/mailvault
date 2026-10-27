package state

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteConnectionPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	// The production writable pool has one connection. Expand only this test
	// pool to verify DSN settings apply to every newly opened connection.
	if got := store.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("writable pool limit = %d, want 1", got)
	}
	store.db.SetMaxOpenConns(2)
	first, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, query := range []struct {
		sql  string
		want int
	}{{"PRAGMA foreign_keys", 1}, {"PRAGMA busy_timeout", 5000}} {
		for _, conn := range []*sql.Conn{first, second} {
			var got int
			if err := conn.QueryRowContext(ctx, query.sql).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != query.want {
				t.Fatalf("%s = %d, want %d", query.sql, got, query.want)
			}
		}
	}
	for _, conn := range []*sql.Conn{first, second} {
		var mode string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
			t.Fatalf("journal mode = %q, %v", mode, err)
		}
	}
	ro, err := NewReadOnlyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var queryOnly int
	if err := ro.db.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil {
		t.Fatal(err)
	}
	if queryOnly != 1 {
		t.Fatalf("query_only = %d", queryOnly)
	}
	if _, err := ro.db.Exec("CREATE TABLE forbidden (id INTEGER)"); err == nil {
		t.Fatal("read-only store accepted a write")
	}
}

func TestLegacyMattnDatabaseCompatibility(t *testing.T) {
	data, err := os.ReadFile("testdata/legacy-mattn.db")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	exists, err := store.DeliveryExists(ctx, "fixture-target", "fixture-identity")
	if err != nil || !exists {
		t.Fatalf("legacy receipt: %v, %v", exists, err)
	}
	messages, err := store.FindByContentHash(ctx, "fixture-hash")
	if err != nil || len(messages) != 1 {
		t.Fatalf("legacy messages: %v, %v", messages, err)
	}
	if messages[0].FetchedAt.Year() != 2026 {
		t.Fatalf("legacy timestamp: %v", messages[0].FetchedAt)
	}
	if err := store.MarkDelivered(ctx, "new-target", "new-identity", "fixture", "INBOX", "1"); err != nil {
		t.Fatal(err)
	}
	ro, err := NewReadOnlyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	exists, err = ro.DeliveryExists(ctx, "new-target", "new-identity")
	if err != nil || !exists {
		t.Fatalf("new receipt: %v, %v", exists, err)
	}
}
