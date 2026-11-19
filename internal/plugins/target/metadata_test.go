package target

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMaildirRefreshFlagsKeepsBytesAndResolvesOldLocator(t *testing.T) {
	value, err := NewMaildirTarget(map[string]interface{}{"path": physicalTempDir(t), "preserve_flags": true})
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "Windows does not support") {
			t.Fatalf("unsupported Maildir flag preservation was not rejected: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	target := value.(*MaildirTarget)
	ctx := context.Background()
	if err := target.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	msg := &plugins.Message{Raw: []byte("Subject: example\r\n\r\nbody\r\n"), Flags: []string{"\\Seen"}}
	locator, err := target.WriteBackupMessage(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	entry := plugins.BackupMessage{Locator: locator, RawSHA256: dedup.ComputeContentHash(msg.Raw)}
	updated, err := target.UpdateBackupMetadata(ctx, entry, []string{"\\Flagged", "custom"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(updated, ":2,F") {
		t.Fatalf("flags not updated: %s", updated)
	}
	read, err := target.ReadBackupMessage(ctx, locator)
	if err != nil || string(read.Raw) != string(msg.Raw) {
		t.Fatalf("old locator read: %v", err)
	}
	entry.Locator = updated
	updated, err = target.UpdateBackupMetadata(ctx, entry, nil)
	if err != nil || !strings.HasSuffix(updated, ":2,") {
		t.Fatalf("clear flags: %s %v", updated, err)
	}
	files, err := os.ReadDir(filepath.Join(target.path, "cur"))
	if err != nil || len(files) != 1 {
		t.Fatalf("copies: %d %v", len(files), err)
	}
	entry.RawSHA256 = "invalid"
	if _, err := target.UpdateBackupMetadata(ctx, entry, []string{"\\Seen"}); err == nil {
		t.Fatal("corrupt message accepted")
	}
}

func TestMaildirKeywordsDoNotSetSystemFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Maildir flag filenames are unsupported on Windows")
	}
	value, err := NewMaildirTarget(map[string]interface{}{"path": physicalTempDir(t), "preserve_flags": true})
	if err != nil {
		t.Fatal(err)
	}
	target := value.(*MaildirTarget)
	t.Cleanup(func() { _ = target.Close() })
	ctx := context.Background()
	if err := target.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	keywords := []string{"seen", "Answered", "flagged", "DELETED", "draft"}
	msg := &plugins.Message{Raw: []byte("Subject: fixture\r\n\r\nbody\r\n"), Flags: keywords}
	locator, err := target.WriteBackupMessage(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.ToSlash(locator), "new/") || strings.Contains(locator, ":2,") {
		t.Errorf("keyword-only message locator = %q, want unflagged file in new/", locator)
	}
	entry := plugins.BackupMessage{Locator: locator, RawSHA256: dedup.ComputeContentHash(msg.Raw)}
	updated, err := target.UpdateBackupMetadata(ctx, entry, append(keywords, `\sEeN`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(updated, ":2,S") {
		t.Errorf("refreshed locator = %q, want only the Seen flag", updated)
	}
}

func TestMaildirRefreshKeepsUnflaggedMessagesInNew(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Maildir flag filenames are unsupported on Windows")
	}
	root := physicalTempDir(t)
	value, err := NewMaildirTarget(map[string]interface{}{"path": root, "preserve_flags": true})
	if err != nil {
		t.Fatal(err)
	}
	target := value.(*MaildirTarget)
	t.Cleanup(func() { _ = target.Close() })
	ctx := context.Background()
	if err := target.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	msg := &plugins.Message{Raw: []byte("Subject: fixture\r\n\r\nbody\r\n")}
	locator, err := target.WriteBackupMessage(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(locator) != "new" {
		t.Fatalf("unflagged message locator = %q, want file in new/", locator)
	}
	entry := plugins.BackupMessage{Locator: locator, RawSHA256: dedup.ComputeContentHash(msg.Raw)}
	updated, err := target.UpdateBackupMetadata(ctx, entry, []string{"project"})
	if err != nil || updated != filepath.ToSlash(locator) {
		t.Fatalf("refreshed locator = %q, err = %v, want %q", updated, err, locator)
	}
	if _, err := os.Stat(filepath.Join(root, locator)); err != nil {
		t.Fatalf("message disappeared from new/: %v", err)
	}
}
