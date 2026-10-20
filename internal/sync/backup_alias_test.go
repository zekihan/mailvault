package sync

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/state"
)

func TestRecordBackupAliasUsesHashMatchesAndPlacementOrder(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		target plugins.Target
		folder string
		want   string
	}{
		{name: "flat uses first deterministic candidate", target: &fixtureTarget{name: "first"}, folder: "Starred", want: "a-inbox"},
		{name: "preserved placement selects matching folder", target: &placementTarget{&fixtureTarget{name: "first"}}, folder: "Starred", want: "z-starred"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storePath := filepath.Join(t.TempDir(), "state.db")
			store, err := state.NewStore(storePath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			source := &fixtureSource{name: "account"}
			const hash = "same-hash"
			for _, row := range []struct{ source, folder, locator, target, rowHash string }{
				{source: "z-source", folder: "Starred", locator: "z-starred", target: "first", rowHash: hash},
				{source: "a-source", folder: "INBOX", locator: "a-inbox", target: "first", rowHash: hash},
				{source: "0-other-target", folder: "Starred", locator: "wrong-target", target: "second", rowHash: hash},
				{source: "0-unrelated", folder: "Starred", locator: "unrelated", target: "first", rowHash: "other-hash"},
			} {
				if err := store.RecordBackupMessage(ctx, plugins.BackupMessage{Source: row.source, Target: row.target, Scope: "scope", Folder: row.folder, UID: "1", Locator: row.locator, RawSHA256: row.rowHash}); err != nil {
					t.Fatal(err)
				}
			}
			// A malformed unrelated row used to make loading the whole manifest fail.
			db, err := sql.Open("sqlite", storePath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.ExecContext(ctx, `INSERT INTO backup_messages(source_name,target_name,scope,folder,uid,locator,raw_sha256,flags_json) VALUES(?,?,?,?,?,?,?,?)`, "broken", "first", "scope", "INBOX", "2", "broken", "other-hash", "{"); err != nil {
				t.Fatal(err)
			}
			msg := &plugins.Message{UID: "2", UIDScope: "scope", Folder: tc.folder}
			if err := (&SyncEngine{stateStore: store}).recordBackupAlias(ctx, source, tc.target, msg, hash); err != nil {
				t.Fatalf("record alias: %v", err)
			}
			entry, found, err := store.FindBackupMessage(ctx, "account", plugins.TargetStateKey(tc.target), targetReceiptScope(tc.target, "scope"), tc.folder, "2")
			if err != nil || !found {
				t.Fatalf("find alias: found=%t err=%v", found, err)
			}
			if entry.Locator != tc.want {
				t.Fatalf("alias locator = %q, want %q", entry.Locator, tc.want)
			}
		})
	}
}

func TestRecordBackupAliasWithoutMatchingPlacementIsNoOp(t *testing.T) {
	ctx := context.Background()
	f := newEngineFixture(t)
	if err := f.store.RecordBackupMessage(ctx, plugins.BackupMessage{Source: "account", Target: "first", Scope: "scope", Folder: "INBOX", UID: "1", Locator: "inbox", RawSHA256: "hash"}); err != nil {
		t.Fatal(err)
	}
	target := &placementTarget{&fixtureTarget{name: "first"}}
	if err := (&SyncEngine{stateStore: f.store}).recordBackupAlias(ctx, f.source, target, &plugins.Message{UID: "2", UIDScope: "scope", Folder: "Starred"}, "hash"); err != nil {
		t.Fatal(err)
	}
	entries, err := f.store.BackupMessages(ctx, "first")
	if err != nil || len(entries) != 1 {
		t.Fatalf("manifest after unmatched alias: %d entries, %v; want original only", len(entries), err)
	}
}
