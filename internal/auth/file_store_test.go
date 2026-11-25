package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFileStore(t *testing.T, store FileStore, account, state string) {
	t.Helper()
	if err := store.Set(t.Context(), account, state); err != nil {
		t.Fatal(err)
	}
}

func checkFileStorePrivate(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestFileStoreWritesPrivateAtomicTokenState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "tokens.json")
	store := FileStore{Path: path}
	writeFileStore(t, store, "account", `{"refresh_token":"fixture-secret"}`)
	checkFileStorePrivate(t, path)
	got, err := store.Get(t.Context(), "account")
	if err != nil || got != `{"refresh_token":"fixture-secret"}` {
		t.Fatalf("Get() = %q, %v", got, err)
	}
	checkFileStoreRejectsInsecure(t, store, path)
	checkFileStoreRejectsOtherAccount(t, store)
	checkFileStoreRestorePermissions(t, path)
	writeFileStore(t, store, "account", `{"refresh_token":"rotated-secret"}`)
	checkFileStorePrivateReplaced(t, path)
}

func checkFileStoreRejectsInsecure(t *testing.T, store FileStore, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil { // #nosec G302 -- insecure perms are the test scenario: verifies rejection of weak credential files
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), "account"); err == nil {
		t.Fatal("accepted token file with group/world permissions")
	}
	if err := store.Set(t.Context(), "account", `{"refresh_token":"rotated-secret"}`); err == nil {
		t.Fatal("overwrote an insecure token file")
	}
}

func checkFileStoreRejectsOtherAccount(t *testing.T, store FileStore) {
	t.Helper()
	if _, err := store.Get(t.Context(), "another-account"); err == nil {
		t.Fatal("returned another account's token state")
	}
	if err := store.Set(t.Context(), "another-account", `{"refresh_token":"wrong-account"}`); err == nil {
		t.Fatal("overwrote another account's token state")
	}
}

func checkFileStoreRestorePermissions(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkFileStorePrivateReplaced(t *testing.T, path string) {
	t.Helper()
	checkFileStorePrivate(t, path)
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("replacement permissions = %v, %v", info, err)
	}
}

func TestFileStoreRejectsSymlinksAndWritableDirectories(t *testing.T) {
	dir := t.TempDir()
	store := FileStore{Path: filepath.Join(dir, "tokens.json")}
	if err := store.Set(t.Context(), "account", "fixture-state"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(store.Path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileStore{Path: link}).Get(t.Context(), "account"); err == nil {
		t.Fatal("followed token file symlink")
	}
	if err := os.Chmod(dir, 0o777); err != nil { // #nosec G302 -- insecure perms are the test scenario: verifies rejection of weak credential files
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), "account"); err == nil {
		t.Fatal("read token from group/world writable directory")
	}
}
