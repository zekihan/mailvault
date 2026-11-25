package target

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

type refreshFlagsFixture struct {
	target *MaildirTarget
	msg    *plugins.Message
}

func (fixture *refreshFlagsFixture) open(t *testing.T) {
	t.Helper()
	value, err := NewMaildirTarget(map[string]any{"path": physicalTempDir(t), "preserve_flags": true})
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "Windows does not support") {
			t.Fatalf("unsupported Maildir flag preservation was not rejected: %v", err)
		}
		t.Skip("Maildir flag filenames are unsupported on Windows")
	}
	if err != nil {
		t.Fatal(err)
	}
	fixture.target = mustAssert[*MaildirTarget](t, value)
	if err := fixture.target.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func (fixture *refreshFlagsFixture) entry(t *testing.T) plugins.BackupMessage {
	t.Helper()
	fixture.msg = &plugins.Message{Raw: []byte("Subject: example\r\n\r\nbody\r\n"), Flags: []string{"\\Seen"}}
	locator, err := fixture.target.WriteBackupMessage(t.Context(), fixture.msg)
	if err != nil {
		t.Fatal(err)
	}
	return plugins.BackupMessage{Locator: locator, RawSHA256: dedup.ComputeContentHash(fixture.msg.Raw)}
}

func (fixture *refreshFlagsFixture) refresh(t *testing.T, entry *plugins.BackupMessage, flags []string) string {
	t.Helper()
	updated, err := fixture.target.UpdateBackupMetadata(t.Context(), entry, flags)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func (fixture *refreshFlagsFixture) checkFlagUpdate(t *testing.T, entry *plugins.BackupMessage) string {
	t.Helper()
	updated := fixture.refresh(t, entry, []string{"\\Flagged", "custom"})
	if !strings.HasSuffix(updated, ":2,F") {
		t.Fatalf("flags not updated: %s", updated)
	}
	read, err := fixture.target.ReadBackupMessage(t.Context(), entry.Locator)
	if err != nil || !bytes.Equal(read.Raw, fixture.msg.Raw) {
		t.Fatalf("old locator read: %v", err)
	}
	return updated
}

func (fixture *refreshFlagsFixture) checkCleared(t *testing.T, updated string) {
	t.Helper()
	entry := plugins.BackupMessage{Locator: updated, RawSHA256: dedup.ComputeContentHash(fixture.msg.Raw)}
	updated, err := fixture.target.UpdateBackupMetadata(t.Context(), &entry, nil)
	if err != nil || !strings.HasSuffix(updated, ":2,") {
		t.Fatalf("clear flags: %s %v", updated, err)
	}
	files, err := os.ReadDir(filepath.Join(fixture.target.path, "cur"))
	if err != nil || len(files) != 1 {
		t.Fatalf("copies: %d %v", len(files), err)
	}
	entry.RawSHA256 = "invalid"
	if _, err := fixture.target.UpdateBackupMetadata(t.Context(), &entry, []string{"\\Seen"}); err == nil {
		t.Fatal("corrupt message accepted")
	}
}

func TestMaildirRefreshFlagsKeepsBytesAndResolvesOldLocator(t *testing.T) {
	fixture := &refreshFlagsFixture{}
	fixture.open(t)
	entry := fixture.entry(t)
	updated := fixture.checkFlagUpdate(t, &entry)
	fixture.checkCleared(t, updated)
}

func TestMaildirKeywordsDoNotSetSystemFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Maildir flag filenames are unsupported on Windows")
	}
	value, err := NewMaildirTarget(map[string]any{"path": physicalTempDir(t), "preserve_flags": true})
	if err != nil {
		t.Fatal(err)
	}
	target := mustAssert[*MaildirTarget](t, value)
	t.Cleanup(func() { _ = target.Close() })
	ctx := t.Context()
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
	updated, err := target.UpdateBackupMetadata(ctx, &entry, append(keywords, `\sEeN`))
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
	value, err := NewMaildirTarget(map[string]any{"path": root, "preserve_flags": true})
	if err != nil {
		t.Fatal(err)
	}
	target := mustAssert[*MaildirTarget](t, value)
	t.Cleanup(func() { _ = target.Close() })
	ctx := t.Context()
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
	updated, err := target.UpdateBackupMetadata(ctx, &entry, []string{"project"})
	if err != nil || updated != filepath.ToSlash(locator) {
		t.Fatalf("refreshed locator = %q, err = %v, want %q", updated, err, locator)
	}
	if _, err := os.Stat(filepath.Join(root, locator)); err != nil {
		t.Fatalf("message disappeared from new/: %v", err)
	}
}
