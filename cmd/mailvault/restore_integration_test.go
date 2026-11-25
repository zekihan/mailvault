package main

import (
	"bytes"
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

type backupWriter interface {
	WriteBackupMessage(ctx context.Context, msg *plugins.Message) (string, error)
}

func setupMaildirRestoreFixture(ctx context.Context, t *testing.T) (backupWriter, plugins.TargetBackupReader, string, string, string) {
	t.Helper()
	maildirPath := physicalTempDir(t) + "/backup"
	backup, err := target.NewMaildirTarget(map[string]any{"path": maildirPath, "max_listing_entries": 64, "max_listing_locator_bytes": 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backup.Close() })
	mustAssert[interface{ SetName(name string) }](t, backup).SetName("archive")
	backupStateKey := plugins.TargetStateKey(backup)
	if err := backup.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	writer := mustAssert[backupWriter](t, backup)
	raw := []byte("From: test@example.com\r\nMessage-ID: <restore@example.com>\r\n\r\nbody\r\n")
	oldLocator, err := writer.WriteBackupMessage(ctx, &plugins.Message{Raw: []byte("damaged original")})
	if err != nil {
		t.Fatal(err)
	}
	currentLocator, err := writer.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	renameMaildirFixture(t, maildirPath, currentLocator)
	reader := mustAssert[plugins.TargetBackupReader](t, backup)
	physical, err := reader.ListBackupMessages(ctx)
	if err != nil || len(physical) != 2 {
		t.Fatalf("physical messages = %d, err = %v", len(physical), err)
	}
	return writer, reader, backupStateKey, oldLocator, currentLocator
}

func renameMaildirFixture(t *testing.T, maildirPath, currentLocator string) {
	t.Helper()
	currentPath := filepath.Join(maildirPath, filepath.FromSlash(currentLocator))
	renamedPath := filepath.Join(maildirPath, "cur", filepath.Base(currentPath)+":2,S")
	if err := os.Rename(currentPath, renamedPath); err != nil {
		t.Fatal(err)
	}
}

func seedMaildirRestoreManifest(ctx context.Context, t *testing.T, store *state.Store, backupStateKey string, entry *plugins.BackupMessage, oldLocator, currentLocator string) plugins.BackupMessage {
	t.Helper()
	alias := *entry
	alias.UID = "43"
	if err := store.RecordBackupMessage(ctx, &alias); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkBackupObjectSuperseded(ctx, backupStateKey, oldLocator, currentLocator); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBackupMessage(ctx, entry); err != nil {
		t.Fatal(err)
	}
	return *entry
}

func runMaildirRestore(ctx context.Context, t *testing.T, reader plugins.TargetBackupReader, backupStateKey string, store *state.Store) (*recordingMailbox, restore.Result) {
	t.Helper()
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
	return appender, result
}

func assertMaildirRestoreMetadata(t *testing.T, appender *recordingMailbox, entry *plugins.BackupMessage) {
	t.Helper()
	if len(appender.appended) != 1 {
		t.Fatalf("appended = %d", len(appender.appended))
	}
	message := appender.appended[0]
	if message.UID != entry.UID || message.UIDScope != entry.Scope || message.Folder != entry.Folder || message.InternalDate != entry.InternalDate || len(message.Flags) != 2 || message.Flags[1] != "$Label1" {
		t.Fatalf("restored metadata = %#v", message)
	}
}

func recordRepairedEntry(ctx context.Context, t *testing.T, store *state.Store, writer backupWriter, entry *plugins.BackupMessage, raw []byte) {
	t.Helper()
	repairedLocator, err := writer.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	repaired := *entry
	repaired.Source = "renamed account"
	repaired.Scope = "renamed scope"
	repaired.Folder = "Archive"
	repaired.UID = "99"
	repaired.Locator = repairedLocator
	if err := store.RecordBackupMessage(ctx, &repaired); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreReadsManifestBackedMaildirOnceAndPreservesMetadata(t *testing.T) {
	ctx := t.Context()
	writer, reader, backupStateKey, oldLocator, currentLocator := setupMaildirRestoreFixture(ctx, t)
	raw := []byte("From: test@example.com\r\nMessage-ID: <restore@example.com>\r\n\r\nbody\r\n")
	digest := sha256.Sum256(raw)
	entry := plugins.BackupMessage{Source: "account", Target: backupStateKey, Scope: "scope", Folder: "INBOX", UID: "42", Locator: currentLocator, RawSHA256: hex.EncodeToString(digest[:]), Flags: []string{"\\Seen", "$Label1"}, InternalDate: "2026-10-20T12:00:00Z"}
	store, err := state.NewStore(t.Context(), physicalTempDir(t)+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entry = seedMaildirRestoreManifest(ctx, t, store, backupStateKey, &entry, oldLocator, currentLocator)
	appender, result := runMaildirRestore(ctx, t, reader, backupStateKey, store)
	if result.Restored != 1 || len(appender.appended) != 1 {
		t.Fatalf("result = %#v, appended = %d", result, len(appender.appended))
	}
	assertMaildirRestoreMetadata(t, appender, &entry)
	recordRepairedEntry(ctx, t, store, writer, &entry, raw)
	resumed, result := runMaildirRestore(ctx, t, reader, backupStateKey, store)
	if result.Restored != 0 || result.Skipped != 2 || len(resumed.appended) != 0 {
		t.Fatalf("repair rerun: result=%#v appended=%d", result, len(resumed.appended))
	}
}

func TestRestoreMboxManifestAfterEscapingPreservesExactBytes(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(physicalTempDir(t), "backup.mbox")
	backup, err := target.NewMboxTarget(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	mustAssert[interface{ SetName(name string) }](t, backup).SetName("archive")
	backupStateKey := plugins.TargetStateKey(backup)
	if err := backup.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: test@example.com\r\nMessage-ID: <mbox-restore@example.com>\r\n\r\nbody\r\nFrom should be escaped\n>From already escaped\nlast line without newline")
	writer := mustAssert[interface {
		WriteBackupMessage(ctx context.Context, msg *plugins.Message) (string, error)
	}](t, backup)
	locator, err := writer.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	reader := mustAssert[plugins.TargetBackupReader](t, backup)
	physical, err := reader.ListBackupMessages(ctx)
	if err != nil || len(physical) != 1 {
		t.Fatalf("physical messages = %d, err = %v", len(physical), err)
	}
	digest := sha256.Sum256(raw)
	entry := plugins.BackupMessage{Source: "account", Target: backupStateKey, Scope: "scope", Folder: "INBOX", UID: "77", Locator: locator, RawSHA256: hex.EncodeToString(digest[:])}
	store, err := state.NewStore(t.Context(), filepath.Join(physicalTempDir(t), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RecordBackupMessage(ctx, &entry); err != nil {
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
	if result.Restored != 1 || len(appender.appended) != 1 || !bytes.Equal(appender.appended[0].Raw, raw) {
		t.Fatalf("restored result=%+v message=%+v", result, appender.appended)
	}
}
