//go:build unix

package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMaildirRejectsSymlinkedStructure(t *testing.T) {
	for _, component := range []string{"root", "ancestor", "tmp", "new", "cur"} {
		for _, create := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/create=%v", component, create), func(t *testing.T) {
				base, outside := t.TempDir(), t.TempDir()
				root := filepath.Join(base, "maildir")
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"tmp", "new", "cur"} {
					if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
						t.Fatal(err)
					}
				}
				switch component {
				case "root":
					root = filepath.Join(base, "link")
					if err := os.Symlink(outside, root); err != nil {
						t.Fatal(err)
					}
				case "ancestor":
					link := filepath.Join(base, "link")
					if err := os.Symlink(base, link); err != nil {
						t.Fatal(err)
					}
					root = filepath.Join(link, "maildir")
				default:
					path := filepath.Join(root, component)
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, path); err != nil {
						t.Fatal(err)
					}
				}
				plugin, err := NewMaildirTarget(map[string]interface{}{"path": root, "create_if_missing": create})
				if err != nil {
					t.Fatal(err)
				}
				defer plugin.Close()
				if err := plugin.Initialize(context.Background()); err == nil {
					t.Fatal("accepted symlink in Maildir structure")
				}
				entries, err := os.ReadDir(outside)
				if err != nil || len(entries) != 0 {
					t.Fatalf("modified outside directory: %v, %v", entries, err)
				}
			})
		}
	}
}

func TestMaildirRejectsSymlinkedPreservedFolderComponents(t *testing.T) {
	for _, level := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			target := durableMaildirFixture(t)
			defer target.Close()
			target.preserveFolders = true
			path, err := target.messagePath("parent/child")
			if err != nil {
				t.Fatal(err)
			}
			components := []string{filepath.Join(target.path, ".folders"), filepath.Dir(path), path}
			if err := os.MkdirAll(filepath.Dir(components[level]), 0700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.Symlink(outside, components[level]); err != nil {
				t.Fatal(err)
			}
			if err := target.WriteMessage(context.Background(), &plugins.Message{Folder: "parent/child", Raw: []byte("fixture")}); err == nil {
				t.Fatal("accepted symlink in preserved folder")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("wrote outside root: %v, %v", entries, err)
			}
		})
	}
}

func TestMaildirDirectoryReplacementDuringPublicationStaysOnDescriptors(t *testing.T) {
	for _, component := range []string{"root", "tmp", "new", "cur", "folder"} {
		t.Run(component, func(t *testing.T) {
			target := durableMaildirFixture(t)
			defer target.Close()
			message := &plugins.Message{Raw: []byte("fixture")}
			replaced := filepath.Join(target.path, component)
			if component == "root" {
				replaced = target.path
			}
			if component == "cur" {
				target.preserveFlags = true
				message.Flags = []string{"seen"}
			}
			if component == "folder" {
				target.preserveFolders = true
				message.Folder = "parent/child"
				path, err := target.messagePath(message.Folder)
				if err != nil {
					t.Fatal(err)
				}
				replaced = filepath.Dir(path)
			}
			outside := t.TempDir()
			target.link = func(from *maildirDirectory, oldName string, to *maildirDirectory, newName string) error {
				if err := os.Rename(replaced, replaced+"-old"); err != nil {
					return err
				}
				if err := os.Symlink(outside, replaced); err != nil {
					return err
				}
				return linkMaildirFile(from, oldName, to, newName)
			}
			locator, err := target.WriteBackupMessage(context.Background(), message)
			if err == nil || locator != "" || target.LastBackupLocator() != "" {
				t.Fatalf("acknowledged changed directory: locator=%q err=%v", locator, err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("publication escaped root: %v, %v", entries, err)
			}
			// Both rollback and temporary cleanup must operate on the old handles.
			oldRoot := target.path
			if component == "root" {
				oldRoot += "-old"
			}
			err = filepath.WalkDir(oldRoot, func(path string, entry os.DirEntry, err error) error {
				if err == nil && entry.Type().IsRegular() {
					t.Errorf("failed publication left a file at %s", path)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMaildirReadsNeverFollowContinuouslySwappedSymlink(t *testing.T) {
	target := durableMaildirFixture(t)
	defer target.Close()
	regular := []byte("Subject: fixture\n\nsafe fixture")
	locator, err := target.WriteBackupMessage(context.Background(), &plugins.Message{Raw: regular})
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("out-of-root fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target.path, locator)
	stop := make(chan struct{})
	failures := make(chan error, 1)
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, symlink := range []bool{true, false} {
				staging := path + "-swap"
				var err error
				if symlink {
					err = os.Symlink(outside, staging)
				} else {
					err = os.WriteFile(staging, regular, 0600)
				}
				if err == nil {
					err = os.Rename(staging, path)
				}
				if err != nil {
					failures <- err
					return
				}
			}
		}
	}()
	for range 200 {
		message, err := target.ReadBackupMessage(context.Background(), locator)
		if err == nil && !bytes.Equal(message.Raw, regular) {
			t.Errorf("read outside root: %q", message.Raw)
			break
		}
	}
	close(stop)
	writer.Wait()
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
}

func TestMaildirReaderPinsInodeAndBoundsOversizedReplacement(t *testing.T) {
	target := durableMaildirFixture(t)
	defer target.Close()
	locator, err := target.WriteBackupMessage(context.Background(), &plugins.Message{Raw: []byte("safe fixture")})
	if err != nil {
		t.Fatal(err)
	}
	file, _, err := target.root.resolveMessage(locator)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	path := filepath.Join(target.path, locator)
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	oversized, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := oversized.Truncate(maxBackupMessageReadBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := oversized.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := readMaildirMessage(file, 128)
	if err != nil || string(raw) != "safe fixture" {
		t.Fatalf("reopened replaced pathname: %q, %v", raw, err)
	}
	if _, err := target.ReadBackupMessage(context.Background(), locator); err == nil {
		t.Fatal("accepted oversized replacement")
	}
	if _, err := target.ReadBackupMessage(context.Background(), "../outside"); err == nil {
		t.Fatal("accepted escaping locator")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := target.ReadBackupMessage(context.Background(), locator); !errors.Is(err, plugins.ErrBackupMessageMissing) {
		t.Fatalf("missing message error: %v", err)
	}
}

func TestMaildirRejectsRootReplacementDuringPublicationSync(t *testing.T) {
	target := durableMaildirFixture(t)
	defer target.Close()
	outside := t.TempDir()
	replaced := false
	target.syncDir = func(string) error {
		if !replaced {
			replaced = true
			if err := os.Rename(target.path, target.path+"-old"); err != nil {
				return err
			}
			return os.Symlink(outside, target.path)
		}
		return nil
	}
	locator, err := target.WriteBackupMessage(context.Background(), &plugins.Message{Raw: []byte("fixture")})
	if err == nil || locator != "" || target.LastBackupLocator() != "" {
		t.Fatalf("acknowledged changed root after sync: locator=%q err=%v", locator, err)
	}
	for _, path := range []string{outside, filepath.Join(target.path+"-old", "new"), filepath.Join(target.path+"-old", "tmp")} {
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 {
			t.Fatalf("unsafe publication/cleanup at %s: %v, %v", path, entries, err)
		}
	}
}
