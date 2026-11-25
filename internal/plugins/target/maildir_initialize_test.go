//go:build unix

package target

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func setupNonDirectoryComponent(t *testing.T, component string) string {
	t.Helper()
	path := filepath.Join(physicalTempDir(t), "maildir")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cur", "new", "tmp"} {
		candidate := filepath.Join(path, name)
		if name == component {
			if err := os.WriteFile(candidate, []byte("preserve fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Mkdir(candidate, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func checkNonDirectoryRejected(t *testing.T, path, component string, create bool) {
	t.Helper()
	plugin, err := NewMaildirTarget(map[string]any{"path": path, "create_if_missing": create})
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.Initialize(t.Context()); err == nil {
		t.Fatal("existing non-directory was accepted")
	}
	data, err := os.ReadFile(filepath.Join(path, component))
	if err != nil || string(data) != "preserve fixture" {
		t.Fatalf("existing data changed: %v", err)
	}
}

func TestMaildirInitializeRejectsExistingNonDirectories(t *testing.T) {
	for _, component := range []string{"cur", "new", "tmp"} {
		for _, create := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/create=%v", component, create), func(t *testing.T) {
				path := setupNonDirectoryComponent(t, component)
				checkNonDirectoryRejected(t, path, component, create)
			})
		}
	}
}

func TestMaildirInitializationRollbackPreservesExistingAndPopulatedDirectories(t *testing.T) {
	path := filepath.Join(physicalTempDir(t), "existing")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(path, "existing-data")
	if err := os.WriteFile(existing, []byte("keep-existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin, err := NewMaildirTarget(map[string]any{"path": path, "create_if_missing": true})
	if err != nil {
		t.Fatal(err)
	}
	target := mustAssert[*MaildirTarget](t, plugin)
	populated := filepath.Join(path, "new", "concurrent-data")
	target.syncDir = func(string) error {
		if err := os.WriteFile(populated, []byte("keep-populated"), 0o600); err != nil {
			t.Fatal(err)
		}
		return errMaildirSyncFixture
	}
	if err := target.Initialize(t.Context()); !errors.Is(err, errMaildirSyncFixture) {
		t.Fatalf("lost original sync failure: %v", err)
	}
	for _, item := range []struct{ path, want string }{{existing, "keep-existing"}, {populated, "keep-populated"}} {
		data, err := os.ReadFile(item.path)
		if err != nil || string(data) != item.want {
			t.Fatalf("rollback destroyed data at %s: %v", item.path, err)
		}
	}
}
