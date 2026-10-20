package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	if err := store.RecordBackupMessage(context.Background(), plugins.BackupMessage{Source: "test-account", Target: "local", Scope: "scope", Folder: "INBOX", UID: "1", Locator: locator, RawSHA256: dedup.ComputeContentHash(raw)}); err != nil {
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
