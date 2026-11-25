package state

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func openLiteralPathStore(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" && strings.Contains(name, "?") {
		t.Skip("question marks are not valid Windows filenames")
	}
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.db")
	store, err := NewStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSource(t.Context(), "account", "imap", "example.invalid", "test-user"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func checkReopenedLiteralPath(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not written to literal path: %v", err)
	}
	for _, open := range []func(string) (*Store, error){
		func(path string) (*Store, error) { return NewStore(t.Context(), path) },
		func(path string) (*Store, error) { return NewReadOnlyStore(t.Context(), path) },
	} {
		reopened, err := open(path)
		if err != nil {
			t.Fatal(err)
		}
		stats, err := reopened.Stats(t.Context())
		closeErr := reopened.Close()
		if err != nil || closeErr != nil || stats["sources"] != 1 {
			t.Fatalf("lost persisted state on reopen: %v %v %v", stats, err, closeErr)
		}
	}
}

func TestStoreUsesLiteralFilesystemPath(t *testing.T) {
	for _, name := range []string{"plain", "space name", "question?tenant", "query?mode=memory", "hash#fragment", "percent%2Fname", "unicode-收件箱"} { //nolint:gosmopolitan // Unicode (Han script) folder fixture; the byte-limit checks require multibyte runes
		t.Run(name, func(t *testing.T) {
			path := openLiteralPathStore(t, name)
			checkReopenedLiteralPath(t, path)
		})
	}
}
