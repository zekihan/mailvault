package state

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStoreUsesLiteralFilesystemPath(t *testing.T) {
	for _, name := range []string{"plain", "space name", "question?tenant", "query?mode=memory", "hash#fragment", "percent%2Fname", "unicode-收件箱"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.Contains(name, "?") {
				t.Skip("question marks are not valid Windows filenames")
			}
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "state.db")
			store, err := NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertSource(context.Background(), "account", "imap", "example.invalid", "test-user"); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("database was not written to literal path: %v", err)
			}
			for _, open := range []func(string) (*Store, error){NewStore, NewReadOnlyStore} {
				reopened, err := open(path)
				if err != nil {
					t.Fatal(err)
				}
				stats, err := reopened.Stats(context.Background())
				closeErr := reopened.Close()
				if err != nil || closeErr != nil || stats["sources"] != 1 {
					t.Fatalf("lost persisted state on reopen: %v %v %v", stats, err, closeErr)
				}
			}
		})
	}
}
