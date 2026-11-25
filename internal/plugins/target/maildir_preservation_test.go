package target

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMaildirPreservesFoldersAndFlagsWhenEnabled(t *testing.T) {
	root := physicalTempDir(t)
	target, err := NewMaildirTarget(map[string]any{
		"path": root, "preserve_folders": true, "preserve_flags": true,
	})
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "Windows does not support") {
			t.Fatalf("unsupported Maildir flag preservation was not rejected: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	msg := &plugins.Message{Folder: "Archive/2026", Flags: []string{"\\Seen", "\\Flagged", "\\Answered"}, Raw: []byte("mail")}
	if err := target.WriteMessage(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	folderPath, err := mustAssert[*MaildirTarget](t, target).messagePath(msg.Folder)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(folderPath, "cur"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ":2,FRS") {
		t.Fatalf("cur entries = %v, want one message with :2,FRS flags", entries)
	}
	if _, err := os.Stat(filepath.Join(root, "new")); err != nil {
		t.Fatalf("flat root Maildir was not initialized: %v", err)
	}
	if mustAssert[*MaildirTarget](t, target).ReceiptScope() != "maildir-preserve-v1:folders=true:flags=true" {
		t.Fatal("preservation mode did not get a receipt scope")
	}
}

func TestMaildirFolderEncodingIsSafeAndDistinct(t *testing.T) {
	target, err := NewMaildirTarget(map[string]any{"path": physicalTempDir(t), "preserve_folders": true})
	if err != nil {
		t.Fatal(err)
	}
	maildir := mustAssert[*MaildirTarget](t, target)
	for _, folder := range []string{"../outside", "/INBOX", "a//b", "a_b", "a/b"} {
		path, err := maildir.messagePath(folder)
		if err != nil {
			t.Fatalf("messagePath(%q): %v", folder, err)
		}
		rel, err := filepath.Rel(maildir.path, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			t.Errorf("folder %q escaped root: %q, %v", folder, path, err)
		}
	}
	first, _ := maildir.messagePath("a_b")
	second, _ := maildir.messagePath("a/b")
	if first == second {
		t.Fatal("distinct folder labels collided")
	}
	if _, err := NewMaildirTarget(map[string]any{"path": physicalTempDir(t), "preserve_flags": "yes"}); err == nil {
		t.Fatal("invalid preserve_flags type was accepted")
	}
}

func TestMaildirBackupReaderPreservesFolderNames(t *testing.T) {
	for _, folder := range []string{"Archive/2026", "/INBOX", "Archive/", "a//b", "/", "~", "../outside", "受信/保存"} { //nolint:gosmopolitan // Unicode (Han script) folder fixture; the byte-limit checks require multibyte runes
		t.Run(folder, func(t *testing.T) {
			ctx := t.Context()
			target, err := NewMaildirTarget(map[string]any{"path": physicalTempDir(t), "preserve_folders": true})
			if err != nil {
				t.Fatal(err)
			}
			maildir := mustAssert[*MaildirTarget](t, target)
			t.Cleanup(func() { _ = maildir.Close() })
			if err := maildir.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			locator, err := maildir.WriteBackupMessage(ctx, &plugins.Message{Folder: folder, Raw: []byte("Subject: fixture\r\n\r\nbody\r\n")})
			if err != nil {
				t.Fatal(err)
			}
			entries, err := maildir.ListBackupMessages(ctx)
			if err != nil || len(entries) != 1 {
				t.Fatalf("listed %d messages, want one: %v", len(entries), err)
			}
			for _, readLocator := range []string{locator, entries[0].Locator} {
				msg, err := maildir.ReadBackupMessage(ctx, readLocator)
				if err != nil {
					t.Fatal(err)
				}
				if msg.Folder != folder {
					t.Errorf("folder = %q, want %q", msg.Folder, folder)
				}
			}
		})
	}
}

func TestMaildirPreservationDefaultsRemainFlat(t *testing.T) {
	target, err := NewMaildirTarget(map[string]any{"path": physicalTempDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	maildir := mustAssert[*MaildirTarget](t, target)
	if maildir.preserveFolders || maildir.preserveFlags || maildir.ReceiptScope() != "" {
		t.Fatal("omitted preservation settings changed legacy behavior")
	}
	msg := &plugins.Message{Folder: "Sent", Flags: []string{"\\Seen"}}
	if got := maildir.DeliveryIdentity(msg, "hash"); got != "hash" {
		t.Fatalf("legacy delivery identity = %q, want hash", got)
	}
}

func TestMaildirDeliveryIdentityPreservesDuplicateFolderPlacement(t *testing.T) {
	target, err := NewMaildirTarget(map[string]any{"path": physicalTempDir(t), "preserve_folders": true})
	if err != nil {
		t.Fatal(err)
	}
	maildir := mustAssert[*MaildirTarget](t, target)
	base := "hash:identical-content"
	first := maildir.DeliveryIdentity(&plugins.Message{Folder: "INBOX"}, base)
	second := maildir.DeliveryIdentity(&plugins.Message{Folder: "Sent"}, base)
	if first == second {
		t.Fatal("identical content in different folders received the same delivery identity")
	}
	if got := maildir.DeliveryIdentity(&plugins.Message{Folder: "INBOX"}, base); got != first {
		t.Fatalf("same folder identity = %q, want %q", got, first)
	}
}

func TestMaildirWritesDuplicateContentToEachPreservedFolder(t *testing.T) {
	root := physicalTempDir(t)
	target, err := NewMaildirTarget(map[string]any{"path": root, "preserve_folders": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	message := []byte("same message in two folders")
	for _, folder := range []string{"INBOX", "Starred"} {
		if err := target.WriteMessage(t.Context(), &plugins.Message{Folder: folder, Raw: message}); err != nil {
			t.Fatalf("write folder %q: %v", folder, err)
		}
	}
	maildir := mustAssert[*MaildirTarget](t, target)
	for _, folder := range []string{"INBOX", "Starred"} {
		folderPath, err := maildir.messagePath(folder)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Join(folderPath, "new"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("folder %q contains %d messages, want one", folder, len(entries))
		}
		got, err := os.ReadFile(filepath.Join(folderPath, "new", entries[0].Name()))
		if err != nil || !bytes.Equal(got, message) {
			t.Fatalf("folder %q message = %q, %v", folder, got, err)
		}
	}
}

func TestMaildirOversizedFolderComponentFailsBeforeCreatingFolders(t *testing.T) {
	root := physicalTempDir(t)
	target, err := NewMaildirTarget(map[string]any{"path": root, "preserve_folders": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	err = target.WriteMessage(t.Context(), &plugins.Message{Folder: strings.Repeat("x", 200), Raw: []byte("mail")})
	if err == nil {
		t.Fatal("oversized encoded folder component was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, ".folders")); !os.IsNotExist(err) {
		t.Fatalf("failed folder write created partial folder tree: %v", err)
	}
}

func TestMaildirPreserveFoldersHonorsCreateIfMissingFalse(t *testing.T) {
	root := filepath.Join(physicalTempDir(t), "mail")
	for _, name := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	target, err := NewMaildirTarget(map[string]any{
		"path": root, "create_if_missing": false, "preserve_folders": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	err = target.WriteMessage(t.Context(), &plugins.Message{Folder: "Sent", Raw: []byte("mail")})
	if err == nil {
		t.Fatal("write created a missing preserved folder despite create_if_missing=false")
	}
	if _, err := os.Stat(filepath.Join(root, ".folders")); !os.IsNotExist(err) {
		t.Fatalf("missing folder write changed filesystem: %v", err)
	}
}
