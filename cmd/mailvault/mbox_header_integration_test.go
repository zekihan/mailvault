package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/restore"
	"github.com/zekihan/mailvault/internal/state"
)

func TestMboxMalformedHeadersVerifyAndRestore(t *testing.T) {
	ctx := context.Background()
	previousStateDir := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = previousStateDir })
	backup, err := target.NewMboxTarget(map[string]interface{}{"path": filepath.Join(t.TempDir(), "archive.mbox")})
	if err != nil {
		t.Fatal(err)
	}
	mbox := backup.(*target.MboxTarget)
	mbox.SetName("archive")
	if err := mbox.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	defer mbox.Close()
	// The source may preserve malformed headers. Include mixed line endings,
	// quoted From lines and no final newline to exercise the manifest's raw length.
	raw := []byte("Subject: fixture\r\nFrom forged separator\n>From quoted once\r\n>>From quoted twice\nX-Fixture: value\r\n\r\nFrom body\n>From quoted body\r\nlast line without newline")
	locator, err := mbox.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	key := plugins.TargetStateKey(mbox)
	entry := plugins.BackupMessage{Source: "fixture", Target: key, Scope: "scope", Folder: "INBOX", UID: "1", Locator: locator, RawSHA256: dedup.ComputeContentHash(raw)}
	store, err := state.NewStore(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RecordBackupMessage(ctx, entry); err != nil {
		t.Fatal(err)
	}
	report, err := verifyTargetWithStateTarget(ctx, "archive", key, mbox)
	if err != nil || report.Scanned != 1 || report.Verified != 1 || report.Corrupt != 0 || report.Missing != 0 || report.Unverifiable != 0 {
		t.Fatalf("verify malformed headers: %+v, %v", report, err)
	}
	manifest, err := store.BackupMessages(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	reader := restoreTargetReader{TargetBackupReader: mbox, target: key, manifest: manifest}
	appender := &recordingMailbox{}
	result, err := restore.Run(ctx, reader, appender, store, restore.Options{BackupTarget: key, DestinationSource: "fixture", DestinationFolder: "Restored"})
	if err != nil || result.Messages != 1 || result.Restored != 1 || result.Unverified != 0 || len(appender.appended) != 1 {
		t.Fatalf("restore malformed headers: %+v, appended=%d, %v", result, len(appender.appended), err)
	}
	if !bytes.Equal(appender.appended[0].Raw, raw) {
		t.Fatalf("restore changed raw bytes: got %q, want %q", appender.appended[0].Raw, raw)
	}
}
