package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func (r *recordingMailbox) EnsureMailbox(context.Context, string, bool) (bool, error) {
	r.created = true
	return true, nil
}

func (r *recordingMailbox) AppendMessage(_ context.Context, _ string, msg *plugins.Message) error {
	r.appended = append(r.appended, msg)
	return nil
}

func TestRestoreReadsManifestBackedMaildirOnceAndPreservesMetadata(t *testing.T) {
	ctx := context.Background()
	maildirPath := t.TempDir() + "/backup"
	backup, err := target.NewMaildirTarget(map[string]interface{}{"path": maildirPath})
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
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
	reader := backup.(plugins.TargetBackupReader)
	physical, err := reader.ListBackupMessages(ctx)
	if err != nil || len(physical) != 2 {
		t.Fatalf("physical messages = %d, err = %v", len(physical), err)
	}
	digest := sha256.Sum256(raw)
	entry := plugins.BackupMessage{Source: "account", Target: "archive", Scope: "scope", Folder: "INBOX", UID: "42", Locator: currentLocator, RawSHA256: hex.EncodeToString(digest[:]), Flags: []string{"\\Seen", "$Label1"}, InternalDate: "2026-10-20T12:00:00Z"}
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
	if err := store.MarkBackupObjectSuperseded(ctx, "archive", oldLocator, currentLocator); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBackupMessage(ctx, entry); err != nil {
		t.Fatal(err)
	}
	manifest, err := store.BackupMessages(ctx, "archive")
	if err != nil {
		t.Fatal(err)
	}
	superseded, err := store.SupersededBackupObjects(ctx, "archive")
	if err != nil {
		t.Fatal(err)
	}
	merged := restoreTargetReader{TargetBackupReader: reader, target: "archive", manifest: manifest, superseded: superseded}
	appender := &recordingMailbox{}
	result, err := restore.Run(ctx, merged, appender, store, restore.Options{BackupTarget: "archive", DestinationSource: "destination", DestinationFolder: "Restored"})
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
}
