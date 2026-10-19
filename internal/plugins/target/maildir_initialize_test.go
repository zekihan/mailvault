//go:build unix

package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMaildirInitializeRejectsExistingNonDirectories(t *testing.T) {
	for _, component := range []string{"cur", "new", "tmp"} {
		for _, create := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/create=%v", component, create), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "maildir")
				if err := os.Mkdir(path, 0750); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"cur", "new", "tmp"} {
					candidate := filepath.Join(path, name)
					if name == component {
						if err := os.WriteFile(candidate, []byte("preserve fixture"), 0600); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Mkdir(candidate, 0750); err != nil {
						t.Fatal(err)
					}
				}
				plugin, err := NewMaildirTarget(map[string]interface{}{"path": path, "create_if_missing": create})
				if err != nil {
					t.Fatal(err)
				}
				if err := plugin.Initialize(context.Background()); err == nil {
					t.Fatal("existing non-directory was accepted")
				}
				data, err := os.ReadFile(filepath.Join(path, component))
				if err != nil || string(data) != "preserve fixture" {
					t.Fatalf("existing data changed: %v", err)
				}
			})
		}
	}
}

func TestMaildirInitializationRollbackPreservesExistingAndPopulatedDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(path, 0750); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(path, "existing-data")
	if err := os.WriteFile(existing, []byte("keep-existing"), 0600); err != nil {
		t.Fatal(err)
	}
	plugin, err := NewMaildirTarget(map[string]interface{}{"path": path, "create_if_missing": true})
	if err != nil {
		t.Fatal(err)
	}
	target := plugin.(*MaildirTarget)
	populated := filepath.Join(path, "new", "concurrent-data")
	target.syncDir = func(string) error {
		if err := os.WriteFile(populated, []byte("keep-populated"), 0600); err != nil {
			t.Fatal(err)
		}
		return errMaildirSyncFixture
	}
	if err := target.Initialize(context.Background()); !errors.Is(err, errMaildirSyncFixture) {
		t.Fatalf("lost original sync failure: %v", err)
	}
	for _, item := range []struct{ path, want string }{{existing, "keep-existing"}, {populated, "keep-populated"}} {
		data, err := os.ReadFile(item.path)
		if err != nil || string(data) != item.want {
			t.Fatalf("rollback destroyed data at %s: %v", item.path, err)
		}
	}
}
