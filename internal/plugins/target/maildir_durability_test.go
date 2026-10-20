//go:build unix

package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

var errMaildirSyncFixture = errors.New("synthetic directory sync failure")

func durableMaildirFixture(t *testing.T) *MaildirTarget {
	t.Helper()
	plugin, err := NewMaildirTarget(map[string]interface{}{"path": filepath.Join(t.TempDir(), "maildir"), "create_if_missing": true})
	if err != nil {
		t.Fatal(err)
	}
	target := plugin.(*MaildirTarget)
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestMaildirSyncsPublicationBeforeSuccessAndCleanup(t *testing.T) {
	target := durableMaildirFixture(t)
	var events []string
	target.link = func(from, to string) error { events = append(events, "link"); return os.Link(from, to) }
	target.syncDir = func(path string) error {
		events = append(events, "directory-sync")
		if path != filepath.Join(target.path, "new") {
			t.Fatalf("wrong publication directory: %s", path)
		}
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 1 {
			t.Fatalf("sync before publication: entries=%d err=%v", len(entries), err)
		}
		return nil
	}
	target.removeTemp = func(path string) error { events = append(events, "cleanup"); return os.Remove(path) }
	if err := target.WriteMessage(context.Background(), &plugins.Message{Raw: []byte("fixture")}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "link,directory-sync,cleanup" {
		t.Fatalf("unsafe publication order: %s", got)
	}
}

func TestMaildirSyncFailureFailsAndRollsBackPublication(t *testing.T) {
	target := durableMaildirFixture(t)
	calls := 0
	target.syncDir = func(string) error {
		calls++
		if calls == 1 {
			return errMaildirSyncFixture
		}
		return nil
	}
	err := target.WriteMessage(context.Background(), &plugins.Message{Raw: []byte("fixture")})
	if !errors.Is(err, errMaildirSyncFixture) {
		t.Fatalf("directory sync failure lost: %v", err)
	}
	if calls != 2 {
		t.Fatalf("publication and rollback were not both synced: calls=%d", calls)
	}
	for _, name := range []string{"new", "tmp"} {
		entries, err := os.ReadDir(filepath.Join(target.path, name))
		if err != nil || len(entries) != 0 {
			t.Fatalf("failed publication left %s entries: count=%d err=%v", name, len(entries), err)
		}
	}
	target.syncDir = nil
	if err := target.WriteMessage(context.Background(), &plugins.Message{Raw: []byte("fixture")}); err != nil {
		t.Fatalf("retry did not recover: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(target.path, "new"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("retry published wrong number of copies: %d %v", len(entries), err)
	}
}

func TestMaildirInitializationSyncsCreatedDirectoriesAndParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new-parent", "maildir")
	plugin, err := NewMaildirTarget(map[string]interface{}{"path": path, "create_if_missing": true})
	if err != nil {
		t.Fatal(err)
	}
	target := plugin.(*MaildirTarget)
	synced := map[string]bool{}
	target.syncDir = func(path string) error { synced[path] = true; return nil }
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{filepath.Join(path, "cur"), filepath.Join(path, "new"), filepath.Join(path, "tmp"), path, filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
		if !synced[name] {
			t.Fatalf("created directory or ancestor was not synced: %s", name)
		}
	}
}

func TestMaildirInitializeDoesNotReadSearchOnlyAncestors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission reproduction requires non-root user")
	}
	for _, create := range []bool{false, true} {
		t.Run(fmt.Sprintf("create=%v", create), func(t *testing.T) {
			base := t.TempDir()
			ancestor := filepath.Join(base, "search-only")
			path := filepath.Join(ancestor, "archive")
			if err := os.Mkdir(ancestor, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(path, "cur"), 0750); err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{"new", "tmp"} {
				if err := os.Mkdir(filepath.Join(path, dir), 0750); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(ancestor, 0111); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(ancestor, 0700)
			plugin, err := NewMaildirTarget(map[string]interface{}{"path": path, "create_if_missing": create})
			if err != nil {
				t.Fatal(err)
			}
			if err := plugin.(*MaildirTarget).Initialize(context.Background()); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
		})
	}
}

func TestMaildirInitializationSyncFailureRollsBackOwnedDirectories(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "nested", "maildir")
	plugin, err := NewMaildirTarget(map[string]interface{}{"path": path, "create_if_missing": true})
	if err != nil {
		t.Fatal(err)
	}
	target := plugin.(*MaildirTarget)
	target.syncDir = func(string) error { return errMaildirSyncFixture }
	if err := target.Initialize(context.Background()); !errors.Is(err, errMaildirSyncFixture) {
		t.Fatalf("primary sync error lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "nested")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned dirs not rolled back: %v", err)
	}
	target.syncDir = nil
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "tmp")); err != nil {
		t.Fatalf("retry structure absent: %v", err)
	}
}
