package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileStoreWritesPrivateAtomicTokenState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "tokens.json")
	store := FileStore{Path: path}
	if err := store.Set(context.Background(), "account", `{"refresh_token":"fixture-secret"}`); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("token file mode = %04o, want 0600", info.Mode().Perm())
	}
	got, err := store.Get(context.Background(), "account")
	if err != nil || got != `{"refresh_token":"fixture-secret"}` {
		t.Fatalf("Get() = %q, %v", got, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), "account"); err == nil {
		t.Fatal("accepted token file with group/world permissions")
	}
	if err := store.Set(context.Background(), "account", `{"refresh_token":"rotated-secret"}`); err == nil {
		t.Fatal("overwrote an insecure token file")
	}
	if _, err := store.Get(context.Background(), "another-account"); err == nil {
		t.Fatal("returned another account's token state")
	}
	if err := store.Set(context.Background(), "another-account", `{"refresh_token":"wrong-account"}`); err == nil {
		t.Fatal("overwrote another account's token state")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), "account", `{"refresh_token":"rotated-secret"}`); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("replacement permissions = %v, %v", info, err)
	}
}

func TestFileStoreRejectsSymlinksAndWritableDirectories(t *testing.T) {
	dir := t.TempDir()
	store := FileStore{Path: filepath.Join(dir, "tokens.json")}
	if err := store.Set(context.Background(), "account", "fixture-state"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(store.Path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileStore{Path: link}).Get(context.Background(), "account"); err == nil {
		t.Fatal("followed token file symlink")
	}
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), "account"); err == nil {
		t.Fatal("read token from group/world writable directory")
	}
}
