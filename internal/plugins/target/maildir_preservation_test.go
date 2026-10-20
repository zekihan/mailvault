package target

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMaildirPreservesFoldersAndFlagsWhenEnabled(t *testing.T) {
	root := t.TempDir()
	target, err := NewMaildirTarget(map[string]interface{}{
		"path": root, "preserve_folders": true, "preserve_flags": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	msg := &plugins.Message{Folder: "Archive/2026", Flags: []string{"\\Seen", "\\Flagged", "\\Answered"}, Raw: []byte("mail")}
	if err := target.WriteMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	folderPath, err := target.(*MaildirTarget).messagePath(msg.Folder)
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
	if target.(*MaildirTarget).ReceiptScope() != "maildir-preserve-v1:folders=true:flags=true" {
		t.Fatal("preservation mode did not get a receipt scope")
	}
}

func TestMaildirFolderEncodingIsSafeAndDistinct(t *testing.T) {
	target, err := NewMaildirTarget(map[string]interface{}{"path": t.TempDir(), "preserve_folders": true})
	if err != nil {
		t.Fatal(err)
	}
	maildir := target.(*MaildirTarget)
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
	if _, err := NewMaildirTarget(map[string]interface{}{"path": t.TempDir(), "preserve_flags": "yes"}); err == nil {
		t.Fatal("invalid preserve_flags type was accepted")
	}
}

func TestMaildirPreservationDefaultsRemainFlat(t *testing.T) {
	target, err := NewMaildirTarget(map[string]interface{}{"path": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	maildir := target.(*MaildirTarget)
	if maildir.preserveFolders || maildir.preserveFlags || maildir.ReceiptScope() != "" {
		t.Fatal("omitted preservation settings changed legacy behavior")
	}
	msg := &plugins.Message{Folder: "Sent", Flags: []string{"\\Seen"}}
	if got := maildir.DeliveryIdentity(msg, "hash"); got != "hash" {
		t.Fatalf("legacy delivery identity = %q, want hash", got)
	}
}

func TestMaildirDeliveryIdentityPreservesDuplicateFolderPlacement(t *testing.T) {
	target, err := NewMaildirTarget(map[string]interface{}{"path": t.TempDir(), "preserve_folders": true})
	if err != nil {
		t.Fatal(err)
	}
	maildir := target.(*MaildirTarget)
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

func TestMaildirOversizedFolderComponentFailsBeforeCreatingFolders(t *testing.T) {
	root := t.TempDir()
	target, err := NewMaildirTarget(map[string]interface{}{"path": root, "preserve_folders": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	err = target.WriteMessage(context.Background(), &plugins.Message{Folder: strings.Repeat("x", 200), Raw: []byte("mail")})
	if err == nil {
		t.Fatal("oversized encoded folder component was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, ".folders")); !os.IsNotExist(err) {
		t.Fatalf("failed folder write created partial folder tree: %v", err)
	}
}

func TestMaildirPreserveFoldersHonorsCreateIfMissingFalse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mail")
	for _, name := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0750); err != nil {
			t.Fatal(err)
		}
	}
	target, err := NewMaildirTarget(map[string]interface{}{
		"path": root, "create_if_missing": false, "preserve_folders": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	err = target.WriteMessage(context.Background(), &plugins.Message{Folder: "Sent", Raw: []byte("mail")})
	if err == nil {
		t.Fatal("write created a missing preserved folder despite create_if_missing=false")
	}
	if _, err := os.Stat(filepath.Join(root, ".folders")); !os.IsNotExist(err) {
		t.Fatalf("missing folder write changed filesystem: %v", err)
	}
}
