//go:build unix

package target

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func setupSymlinkedComponent(t *testing.T, component string) (string, string) {
	t.Helper()
	base, outside := physicalTempDir(t), physicalTempDir(t)
	root := filepath.Join(base, "maildir")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tmp", "new", "cur"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
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
	return root, outside
}

func checkSymlinkRejected(t *testing.T, root, outside string, create bool) {
	t.Helper()
	plugin, err := NewMaildirTarget(map[string]any{"path": root, "create_if_missing": create})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	if err := plugin.Initialize(t.Context()); err == nil {
		t.Fatal("accepted symlink in Maildir structure")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("modified outside directory: %v, %v", entries, err)
	}
}

func TestMaildirRejectsSymlinkedStructure(t *testing.T) {
	for _, component := range []string{"root", "ancestor", "tmp", "new", "cur"} {
		for _, create := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/create=%v", component, create), func(t *testing.T) {
				root, outside := setupSymlinkedComponent(t, component)
				checkSymlinkRejected(t, root, outside, create)
			})
		}
	}
}

func TestMaildirRejectsSymlinkedPreservedFolderComponents(t *testing.T) {
	for _, level := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(level), func(t *testing.T) {
			target := durableMaildirFixture(t)
			defer target.Close()
			target.preserveFolders = true
			path, err := target.messagePath("parent/child")
			if err != nil {
				t.Fatal(err)
			}
			components := []string{filepath.Join(target.path, ".folders"), filepath.Dir(path), path}
			if err := os.MkdirAll(filepath.Dir(components[level]), 0o700); err != nil {
				t.Fatal(err)
			}
			outside := physicalTempDir(t)
			if err := os.Symlink(outside, components[level]); err != nil {
				t.Fatal(err)
			}
			if err := target.WriteMessage(t.Context(), &plugins.Message{Folder: "parent/child", Raw: []byte("fixture")}); err == nil {
				t.Fatal("accepted symlink in preserved folder")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("wrote outside root: %v, %v", entries, err)
			}
		})
	}
}

type publicationReplacementFixture struct {
	target   *MaildirTarget
	message  *plugins.Message
	replaced string
	outside  string
}

func (fixture *publicationReplacementFixture) open(t *testing.T, component string) {
	t.Helper()
	fixture.target = durableMaildirFixture(t)
	fixture.message = &plugins.Message{Raw: []byte("fixture")}
	fixture.replaced = filepath.Join(fixture.target.path, component)
	fixture.configureComponent(t, component)
	fixture.outside = physicalTempDir(t)
	fixture.target.link = func(from *maildirDirectory, oldName string, to *maildirDirectory, newName string) error {
		return fixture.replaceAndLink(from, oldName, to, newName)
	}
}

func (fixture *publicationReplacementFixture) configureComponent(t *testing.T, component string) {
	t.Helper()
	if component != "root" {
		return
	}
	fixture.replaced = fixture.target.path
}

func (fixture *publicationReplacementFixture) replaceAndLink(from *maildirDirectory, oldName string, to *maildirDirectory, newName string) error {
	if err := os.Rename(fixture.replaced, fixture.replaced+"-old"); err != nil {
		return err
	}
	if err := os.Symlink(fixture.outside, fixture.replaced); err != nil {
		return err
	}
	return linkMaildirFile(from, oldName, to, newName)
}

func (fixture *publicationReplacementFixture) prepare(t *testing.T, component string) {
	t.Helper()
	switch component {
	case "cur":
		fixture.target.preserveFlags = true
		fixture.message.Flags = []string{"\\Seen"}
	case "folder":
		fixture.prepareFolder(t)
	}
}

func (fixture *publicationReplacementFixture) prepareFolder(t *testing.T) {
	t.Helper()
	fixture.target.preserveFolders = true
	fixture.message.Folder = "parent/child"
	path, err := fixture.target.messagePath(fixture.message.Folder)
	if err != nil {
		t.Fatal(err)
	}
	fixture.replaced = filepath.Dir(path)
}

func (fixture *publicationReplacementFixture) check(t *testing.T, component string) {
	t.Helper()
	locator, err := fixture.target.WriteBackupMessage(t.Context(), fixture.message)
	if err == nil || locator != "" || fixture.target.LastBackupLocator() != "" {
		t.Fatalf("acknowledged changed directory: locator=%q err=%v", locator, err)
	}
	entries, err := os.ReadDir(fixture.outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("publication escaped root: %v, %v", entries, err)
	}
	checkNoPublicationLeftovers(t, fixture.target.path, component)
}

func checkNoPublicationLeftovers(t *testing.T, targetPath, component string) {
	t.Helper()
	oldRoot := targetPath
	if component == "root" {
		oldRoot += "-old"
	}
	err := filepath.WalkDir(oldRoot, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			t.Errorf("failed publication left a file at %s", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMaildirDirectoryReplacementDuringPublicationStaysOnDescriptors(t *testing.T) {
	for _, component := range []string{"root", "tmp", "new", "cur", "folder"} {
		t.Run(component, func(t *testing.T) {
			fixture := &publicationReplacementFixture{}
			fixture.open(t, component)
			fixture.prepare(t, component)
			defer fixture.target.Close()
			fixture.check(t, component)
		})
	}
}

func TestMaildirReadsNeverFollowContinuouslySwappedSymlink(t *testing.T) {
	target := durableMaildirFixture(t)
	defer target.Close()
	regular := []byte("Subject: fixture\n\nsafe fixture")
	locator, err := target.WriteBackupMessage(t.Context(), &plugins.Message{Raw: regular})
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(physicalTempDir(t), "outside")
	if err := os.WriteFile(outside, []byte("out-of-root fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target.path, locator)
	stop := make(chan struct{})
	failures := make(chan error, 1)
	var writer sync.WaitGroup
	writer.Go(func() {
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
					err = os.WriteFile(staging, regular, 0o600)
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
	})
	for range 200 {
		message, err := target.ReadBackupMessage(t.Context(), locator)
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
	locator, err := target.WriteBackupMessage(t.Context(), &plugins.Message{Raw: []byte("safe fixture")})
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
	if err := oversized.Truncate(plugins.MaxMessageBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := oversized.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := readMaildirMessage(file, 128)
	if err != nil || string(raw) != "safe fixture" {
		t.Fatalf("reopened replaced pathname: %q, %v", raw, err)
	}
	if _, err := target.ReadBackupMessage(t.Context(), locator); err == nil {
		t.Fatal("accepted oversized replacement")
	}
	if _, err := target.ReadBackupMessage(t.Context(), "../outside"); err == nil {
		t.Fatal("accepted escaping locator")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := target.ReadBackupMessage(t.Context(), locator); !errors.Is(err, plugins.ErrBackupMessageMissing) {
		t.Fatalf("missing message error: %v", err)
	}
}

func TestMaildirRejectsRootReplacementDuringPublicationSync(t *testing.T) {
	target := durableMaildirFixture(t)
	defer target.Close()
	outside := physicalTempDir(t)
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
	locator, err := target.WriteBackupMessage(t.Context(), &plugins.Message{Raw: []byte("fixture")})
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
