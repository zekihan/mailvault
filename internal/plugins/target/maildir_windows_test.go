package target

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
	"golang.org/x/sys/windows"
)

func windowsMaildirFixture(t *testing.T, preserveFolders bool) *MaildirTarget {
	t.Helper()
	plugin, err := NewMaildirTarget(map[string]interface{}{
		"path": filepath.Join(t.TempDir(), "maildir"), "preserve_folders": preserveFolders,
	})
	if err != nil {
		t.Fatal(err)
	}
	target := plugin.(*MaildirTarget)
	t.Cleanup(func() {
		if err := target.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return target
}

func windowsJunction(t *testing.T, path, destination string) {
	t.Helper()
	// Directory junctions exercise Windows reparse handling without requiring
	// symlink privilege or changing developer mode on the runner.
	if output, err := exec.Command("cmd", "/c", "mklink", "/J", path, destination).CombinedOutput(); err != nil {
		t.Fatalf("create fixture junction: %v\n%s", err, output)
	}
}

func windowsSymlink(t *testing.T, path, destination string) {
	t.Helper()
	if err := os.Symlink(destination, path); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skip("Windows account lacks symlink privilege; junction fixtures still run")
		}
		t.Fatal(err)
	}
}

func TestWindowsMaildirWriteReadAndMove(t *testing.T) {
	target := windowsMaildirFixture(t, true)
	ctx := context.Background()
	raw := []byte("Subject: native Windows fixture\r\n\r\nbody\r\n")
	locator, err := target.WriteBackupMessage(ctx, &plugins.Message{Folder: "Archive/2026", Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target.path, locator)
	folder := filepath.Dir(filepath.Dir(path))
	moved := filepath.Join(folder, "cur", filepath.Base(path))
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	message, err := target.ReadBackupMessage(ctx, locator)
	if err != nil || !bytes.Equal(message.Raw, raw) || message.Folder != "Archive/2026" {
		t.Fatalf("old locator did not resolve the moved message: %+v, %v", message, err)
	}
	listed, err := target.ListBackupMessages(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed %d moved messages: %v", len(listed), err)
	}
	message, err = target.ReadBackupMessage(ctx, listed[0].Locator)
	if err != nil || !bytes.Equal(message.Raw, raw) {
		t.Fatalf("listed locator did not read the message: %+v, %v", message, err)
	}
	entries, err := os.ReadDir(filepath.Join(folder, "tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary publication was not cleaned up: %v, %v", entries, err)
	}
}

func TestWindowsMaildirPublicationNeverOverwrites(t *testing.T) {
	target := windowsMaildirFixture(t, false)
	from, err := target.root.child("tmp", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer from.Close()
	to, err := target.root.child("new", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer to.Close()
	if err := from.root.WriteFile("source", []byte("new fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := to.root.WriteFile("existing", []byte("keep fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := linkMaildirFile(from, "source", to, "existing"); err == nil {
		t.Fatal("publication replaced an existing destination")
	}
	raw, err := to.root.ReadFile("existing")
	if err != nil || string(raw) != "keep fixture" {
		t.Fatalf("collision changed the existing message: %q, %v", raw, err)
	}
	if err := linkMaildirFile(from, "source", to, "published"); err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := from.root.Lstat("source")
	if err != nil {
		t.Fatal(err)
	}
	publishedInfo, err := to.root.Lstat("published")
	if err != nil || !os.SameFile(sourceInfo, publishedInfo) {
		t.Fatalf("publication did not create a hard link: %v", err)
	}
	message, err := target.ReadBackupMessage(context.Background(), "new/published")
	if err != nil || string(message.Raw) != "new fixture" {
		t.Fatalf("published hard link could not be read: %+v, %v", message, err)
	}
}

func TestWindowsMaildirRejectsReparseDirectories(t *testing.T) {
	for _, kind := range []string{"junction", "symlink"} {
		for _, component := range []string{"root", "ancestor", "tmp", "new", "cur", "folder"} {
			t.Run(kind+"/"+component, func(t *testing.T) {
				target := windowsMaildirFixture(t, true)
				outside := t.TempDir()
				path := filepath.Join(target.path, component)
				switch component {
				case "root":
					if err := target.Close(); err != nil {
						t.Fatal(err)
					}
					path = target.path
					if err := os.Rename(path, path+"-old"); err != nil {
						t.Fatal(err)
					}
				case "ancestor":
					path = filepath.Join(filepath.Dir(target.path), "alias")
				case "folder":
					folder, err := target.messagePath("parent/child")
					if err != nil {
						t.Fatal(err)
					}
					path = filepath.Dir(folder)
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
				default:
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "junction" {
					windowsJunction(t, path, outside)
				} else {
					windowsSymlink(t, path, outside)
				}
				if component == "ancestor" {
					target.path = filepath.Join(path, "maildir")
					if err := target.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if component == "root" || component == "ancestor" {
					if err := target.Initialize(context.Background()); err == nil {
						t.Fatal("initialized a reparse directory")
					}
				} else {
					folder := ""
					if component == "folder" {
						folder = "parent/child"
					}
					if err := target.WriteMessage(context.Background(), &plugins.Message{Folder: folder, Raw: []byte("fixture")}); err == nil {
						t.Fatal("wrote through a reparse directory")
					}
				}
				entries, err := os.ReadDir(outside)
				if err != nil || len(entries) != 0 {
					t.Fatalf("modified the external directory: %v, %v", entries, err)
				}
			})
		}
	}
}

func TestWindowsMaildirReadRejectsExternalSymlink(t *testing.T) {
	target := windowsMaildirFixture(t, false)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("external fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	windowsSymlink(t, filepath.Join(target.path, "new", "message"), outside)
	if message, err := target.ReadBackupMessage(context.Background(), "new/message"); err == nil || message != nil {
		t.Fatalf("read through external symlink: %+v, %v", message, err)
	}
}

func TestWindowsMaildirPublicationRejectsDirectoryReplacement(t *testing.T) {
	target := windowsMaildirFixture(t, false)
	outside := t.TempDir()
	called := false
	target.link = func(from *maildirDirectory, oldName string, to *maildirDirectory, newName string) error {
		called = true
		path := filepath.Join(target.path, "new")
		if err := os.Rename(path, path+"-old"); err != nil {
			// Denying rename of a pinned directory is also fail-closed behavior.
			return err
		}
		windowsJunction(t, path, outside)
		return linkMaildirFile(from, oldName, to, newName)
	}
	locator, err := target.WriteBackupMessage(context.Background(), &plugins.Message{Raw: []byte("fixture")})
	if !called || err == nil || locator != "" || target.LastBackupLocator() != "" {
		t.Fatalf("acknowledged directory replacement: called=%v locator=%q err=%v", called, locator, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("publication escaped root: %v, %v", entries, err)
	}
}

func TestWindowsMaildirSyncFailureRollsBackPublication(t *testing.T) {
	target := windowsMaildirFixture(t, false)
	syncFailure := errors.New("synthetic directory sync failure")
	calls := 0
	target.syncDir = func(string) error {
		calls++
		if calls == 1 {
			return syncFailure
		}
		return nil
	}
	locator, err := target.WriteBackupMessage(context.Background(), &plugins.Message{Raw: []byte("fixture")})
	if !errors.Is(err, syncFailure) || locator != "" || target.LastBackupLocator() != "" || calls != 2 {
		t.Fatalf("acknowledged unsynced publication: calls=%d locator=%q err=%v", calls, locator, err)
	}
	for _, name := range []string{"new", "tmp"} {
		entries, err := os.ReadDir(filepath.Join(target.path, name))
		if err != nil || len(entries) != 0 {
			t.Fatalf("failed publication left %s entries: %v, %v", name, entries, err)
		}
	}
	target.syncDir = nil
	if err := target.WriteMessage(context.Background(), &plugins.Message{Raw: []byte("retry fixture")}); err != nil {
		t.Fatal(err)
	}
}
