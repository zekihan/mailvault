package target

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMaildirRefreshFlagsKeepsBytesAndResolvesOldLocator(t *testing.T) {
	value, err := NewMaildirTarget(map[string]interface{}{"path": t.TempDir(), "preserve_flags": true})
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
