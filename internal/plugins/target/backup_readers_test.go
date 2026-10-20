package target

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMaildirBackupLocatorSurvivesClientFlagRename(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	target, err := NewMaildirTarget(map[string]interface{}{"path": root})
	if err != nil {
		t.Fatal(err)
	}
	maildir := target.(*MaildirTarget)
	if err := maildir.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: fixture\r\n\r\nbody\r\n")
	locator, err := maildir.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, filepath.FromSlash(locator))
	newDir := filepath.Join(root, "cur")
	newPath := filepath.Join(newDir, filepath.Base(old)+":2,S")
	if err := os.Rename(old, newPath); err != nil {
		t.Fatal(err)
	}
	stored, err := maildir.ReadBackupMessage(ctx, locator)
	if err != nil {
		t.Fatal(err)
	}
	if dedup.ComputeContentHash(stored.Raw) != dedup.ComputeContentHash(raw) {
		t.Fatal("renamed Maildir message did not verify")
	}
}

func TestMboxBackupLocatorPreservesExactRawBytes(t *testing.T) {
	ctx := context.Background()
	target, err := NewMboxTarget(map[string]interface{}{"path": filepath.Join(t.TempDir(), "archive.mbox")})
	if err != nil {
		t.Fatal(err)
	}
	mbox := target.(*MboxTarget)
	if err := mbox.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: fixture\r\n\r\nbody From line\r\n>From escaped\r\n>>From double escaped without final newline")
	locator, err := mbox.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := mbox.ReadBackupMessage(ctx, locator)
	if err != nil {
		t.Fatal(err)
	}
	if dedup.ComputeContentHash(stored.Raw) != dedup.ComputeContentHash(raw) {
		t.Fatalf("mbox roundtrip changed raw bytes: %q", stored.Raw)
	}
}

func TestMboxBackupReaderSeparatesMessagesAndRejectsOutOfBoundsLocator(t *testing.T) {
	ctx := context.Background()
	target, err := NewMboxTarget(map[string]interface{}{"path": filepath.Join(t.TempDir(), "archive.mbox")})
	if err != nil {
		t.Fatal(err)
	}
	mbox := target.(*MboxTarget)
	if err := mbox.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	wants := [][]byte{
		[]byte("Message-ID: <one@example.invalid>\r\n\r\nfirst\r\nFrom separator\r\n"),
		[]byte("Message-ID: <two@example.invalid>\r\n\r\nsecond with final newline\r\n"),
	}
	for _, raw := range wants {
		if _, err := mbox.WriteBackupMessage(ctx, &plugins.Message{Raw: raw}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := mbox.ListBackupMessages(ctx)
	if err != nil || len(entries) != len(wants) {
		t.Fatalf("listed %d messages, %v", len(entries), err)
	}
	for i, entry := range entries {
		stored, err := mbox.ReadBackupMessage(ctx, entry.Locator)
		if err != nil || dedup.ComputeContentHash(stored.Raw) != dedup.ComputeContentHash(wants[i]) {
			t.Fatalf("message %d read = %v, %v", i, stored, err)
		}
	}
	if _, err := mbox.ReadBackupMessage(ctx, "0:999999:1:1"); !errors.Is(err, plugins.ErrBackupMessageCorrupt) {
		t.Fatalf("out-of-bounds locator error = %v", err)
	}
}
