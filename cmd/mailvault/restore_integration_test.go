package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/restore"
	"github.com/zekihan/mailvault/internal/state"
)

type recordingMailbox struct {
	created  bool
	appended []*plugins.Message
}

func (r *recordingMailbox) EnsureMailbox(_ context.Context, _ string, allowExisting bool) (bool, error) {
	r.created = true
	return !allowExisting, nil
}

func (r *recordingMailbox) MailboxUIDValidity(context.Context, string) (string, error) {
	return "7", nil
}

func (r *recordingMailbox) AppendMessage(_ context.Context, _ string, msg *plugins.Message, _ string) error {
	r.appended = append(r.appended, msg)
	return nil
}

func TestRestoreReadsManifestBackedMaildirOnceAndPreservesMetadata(t *testing.T) {
	ctx := context.Background()
	maildirPath := t.TempDir() + "/backup"
	backup, err := target.NewMaildirTarget(map[string]interface{}{"path": maildirPath, "max_listing_entries": 64, "max_listing_locator_bytes": 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	backup.(interface{ SetName(string) }).SetName("archive")
	backupStateKey := plugins.TargetStateKey(backup)
	if err := backup.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	writer := backup.(interface {
		WriteBackupMessage(context.Context, *plugins.Message) (string, error)
	})
	raw := []byte("From: test@example.com\r\nMessage-ID: <restore@example.com>\r\n\r\nbody\r\n")
	oldLocator, err := writer.WriteBackupMessage(ctx, &plugins.Message{Raw: []byte("damaged original")})
	if err != nil {
		t.Fatal(err)
	}
	currentLocator, err := writer.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	currentPath := filepath.Join(maildirPath, filepath.FromSlash(currentLocator))
	renamedPath := filepath.Join(maildirPath, "cur", filepath.Base(currentPath)+":2,S")
	if err := os.Rename(currentPath, renamedPath); err != nil {
		t.Fatal(err)
	}
	reader := backup.(plugins.TargetBackupReader)
	physical, err := reader.ListBackupMessages(ctx)
	if err != nil || len(physical) != 2 {
		t.Fatalf("physical messages = %d, err = %v", len(physical), err)
	}
	digest := sha256.Sum256(raw)
	entry := plugins.BackupMessage{Source: "account", Target: backupStateKey, Scope: "scope", Folder: "INBOX", UID: "42", Locator: currentLocator, RawSHA256: hex.EncodeToString(digest[:]), Flags: []string{"\\Seen", "$Label1"}, InternalDate: "2026-10-20T12:00:00Z"}
	store, err := state.NewStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alias := entry
	alias.UID = "43"
	if err := store.RecordBackupMessage(ctx, alias); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkBackupObjectSuperseded(ctx, backupStateKey, oldLocator, currentLocator); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBackupMessage(ctx, entry); err != nil {
		t.Fatal(err)
	}
	manifest, err := store.BackupMessages(ctx, backupStateKey)
	if err != nil {
		t.Fatal(err)
	}
	superseded, err := store.SupersededBackupObjects(ctx, backupStateKey)
	if err != nil {
		t.Fatal(err)
	}
	merged := restoreTargetReader{TargetBackupReader: reader, target: backupStateKey, manifest: manifest, superseded: superseded}
	appender := &recordingMailbox{}
	result, err := restore.Run(ctx, merged, appender, store, restore.Options{BackupTarget: backupStateKey, DestinationSource: "destination", DestinationFolder: "Restored"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Restored != 1 || len(appender.appended) != 1 {
		t.Fatalf("result = %#v, appended = %d", result, len(appender.appended))
	}
	message := appender.appended[0]
	if message.UID != entry.UID || message.UIDScope != entry.Scope || message.Folder != entry.Folder || message.InternalDate != entry.InternalDate || len(message.Flags) != 2 || message.Flags[1] != "$Label1" {
		t.Fatalf("restored metadata = %#v", message)
	}
	repairedLocator, err := writer.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	repaired := entry
	repaired.Source = "renamed account"
	repaired.Scope = "renamed scope"
	repaired.Folder = "Archive"
	repaired.UID = "99"
	repaired.Locator = repairedLocator
	if err := store.RecordBackupMessage(ctx, repaired); err != nil {
		t.Fatal(err)
	}
	manifest, err = store.BackupMessages(ctx, backupStateKey)
	if err != nil {
		t.Fatal(err)
	}
	superseded, err = store.SupersededBackupObjects(ctx, backupStateKey)
	if err != nil {
		t.Fatal(err)
	}
	merged = restoreTargetReader{TargetBackupReader: reader, target: backupStateKey, manifest: manifest, superseded: superseded}
	resumed := &recordingMailbox{}
	result, err = restore.Run(ctx, merged, resumed, store, restore.Options{BackupTarget: backupStateKey, DestinationSource: "destination", DestinationFolder: "Restored"})
	if err != nil || result.Restored != 0 || result.Skipped != 2 || len(resumed.appended) != 0 {
		t.Fatalf("repair rerun: result=%#v appended=%d err=%v", result, len(resumed.appended), err)
	}
}

func TestRestoreMboxManifestAfterEscapingPreservesExactBytes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backup.mbox")
	backup, err := target.NewMboxTarget(map[string]interface{}{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	backup.(interface{ SetName(string) }).SetName("archive")
	backupStateKey := plugins.TargetStateKey(backup)
	if err := backup.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: test@example.com\r\nMessage-ID: <mbox-restore@example.com>\r\n\r\nbody\r\nFrom should be escaped\n>From already escaped\nlast line without newline")
	writer := backup.(interface {
		WriteBackupMessage(context.Context, *plugins.Message) (string, error)
	})
	locator, err := writer.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	reader := backup.(plugins.TargetBackupReader)
	physical, err := reader.ListBackupMessages(ctx)
	if err != nil || len(physical) != 1 {
		t.Fatalf("physical messages = %d, err = %v", len(physical), err)
	}
	digest := sha256.Sum256(raw)
	entry := plugins.BackupMessage{Source: "account", Target: backupStateKey, Scope: "scope", Folder: "INBOX", UID: "77", Locator: locator, RawSHA256: hex.EncodeToString(digest[:])}
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RecordBackupMessage(ctx, entry); err != nil {
		t.Fatal(err)
	}
	manifest, err := store.BackupMessages(ctx, backupStateKey)
	if err != nil {
		t.Fatal(err)
	}
	merged := restoreTargetReader{TargetBackupReader: reader, target: backupStateKey, manifest: manifest}
	appender := &recordingMailbox{}
	result, err := restore.Run(ctx, merged, appender, store, restore.Options{BackupTarget: backupStateKey, DestinationSource: "destination", DestinationFolder: "Restored"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Restored != 1 || len(appender.appended) != 1 || string(appender.appended[0].Raw) != string(raw) {
		t.Fatalf("restored result=%+v message=%+v", result, appender.appended)
	}
}
