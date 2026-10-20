package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/state"
)

func TestVerifyReportsCorruptMaildirWithoutChangingState(t *testing.T) {
	configureCLI(t)
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Targets[0].Config["path"].(string)
	maildir, err := target.NewMaildirTarget(map[string]interface{}{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	md := maildir.(*target.MaildirTarget)
	md.SetName("local")
	if err := md.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: fixture\r\n\r\nbody\r\n")
	locator, err := md.WriteBackupMessage(context.Background(), &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, filepath.FromSlash(locator)), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(stateDir, "state.db")
	store, err := state.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBackupMessage(context.Background(), plugins.BackupMessage{Source: "test-account", Target: plugins.TargetStateKey(md), Scope: "scope", Folder: "INBOX", UID: "1", Locator: locator, RawSHA256: dedup.ComputeContentHash(raw)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	jsonOutput = true
	cmd := newVerifyCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetContext(context.Background())
	err = cmd.RunE(cmd, []string{"local"})
	if err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("verify error = %v", err)
	}
	var report verificationReport
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		t.Fatalf("decode report: %v; output=%s", decodeErr, output.String())
	}
	if report.Corrupt != 1 {
		t.Fatalf("report = %+v", report)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only verification changed the state database")
	}
}

func TestVerifyDoesNotResolveMailboxCredentials(t *testing.T) {
	configureCLI(t)
	path := t.TempDir()
	contents := fmt.Sprintf("sources:\n  - name: unavailable\n    type: imap\n    host: example.invalid\n    username: fixture\n    password_ref: env:MAILVAULT_TEST_ABSENT_PASSWORD\ntargets:\n  - name: local\n    type: maildir\n    config:\n      path: %q\npairs:\n  - source: unavailable\n    targets: [local]\n", path)
	t.Setenv("MAILVAULT_TEST_ABSENT_PASSWORD", "")
	if err := os.WriteFile(cfgFile, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newVerifyCommand()
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	if err := cmd.RunE(cmd, []string{"local"}); err != nil {
		t.Fatalf("offline verify required source credentials: %v", err)
	}
}

type singleBackupReader struct {
	entry plugins.BackupMessage
	raw   []byte
}

func (r singleBackupReader) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	return []plugins.BackupMessage{r.entry}, nil
}

func (r singleBackupReader) ReadBackupMessage(context.Context, string) (*plugins.Message, error) {
	return &plugins.Message{Raw: r.raw}, nil
}

func TestVerifyDoesNotDoubleCountManifestedLegacyReceiptAfterCleanup(t *testing.T) {
	configureCLI(t)
	raw := []byte("Subject: fixture\n\nbody\n")
	hash := dedup.ComputeContentHash(raw)
	store, err := state.NewStore(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSource(context.Background(), "src", "imap", "host", "user"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkMessageFetched(context.Background(), "src", "INBOX", "1", "<id>", hash, int64(len(raw)), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDelivered(context.Background(), "target", "hash:"+hash, "src", "INBOX", "1"); err != nil {
		t.Fatal(err)
	}
	entry := plugins.BackupMessage{Source: "src", Target: "target", Scope: "scope", Folder: "INBOX", UID: "1", Locator: "object-1", RawSHA256: hash}
	if err := store.RecordBackupMessage(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE messages SET fetched_at=datetime('now','-48 hours')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM messages WHERE fetched_at < datetime('now','-24 hours')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := verifyTarget(context.Background(), "target", singleBackupReader{entry: entry, raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified != 1 || report.Unverifiable != 0 {
		t.Fatalf("report = %+v; want one verified object and no duplicate legacy finding", report)
	}
}
