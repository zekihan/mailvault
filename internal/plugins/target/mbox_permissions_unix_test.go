//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package target

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMboxPermissionPolicy(t *testing.T) {
	// Isolate the process-global umask from all other tests and goroutines.
	if os.Getenv("MAILVAULT_TEST_MBOX_UMASK_CHILD") != "1" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMboxPermissionPolicy$", "-test.timeout=30s") // #nosec G204,G702 -- self-spawned test binary; argv fully controlled
		cmd.Env = append(os.Environ(), "MAILVAULT_TEST_MBOX_UMASK_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("permission test: %v\n%s", err, output)
		}
		return
	}
	unix.Umask(0)
	root := t.TempDir()
	assertMode := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %o, want %o", path, got, want)
		}
	}
	initialize := func(path string) {
		t.Helper()
		target, err := NewMboxTarget(map[string]any{"path": path})
		if err != nil {
			t.Fatal(err)
		}
		if err := target.Initialize(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := target.Close(); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "new", "nested", "archive.mbox")
	initialize(path)
	assertMode(path, 0o600)
	assertMode(filepath.Dir(path), 0o700)
	assertMode(filepath.Dir(filepath.Dir(path)), 0o700)
	sharedDir := filepath.Join(root, "shared")
	if err := os.Mkdir(sharedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(sharedDir, "archive.mbox")
	if err := os.WriteFile(shared, nil, 0o640); err != nil { // #nosec G306 -- fixture file for negative-path validation; never a credential
		t.Fatal(err)
	}
	initialize(shared)
	assertMode(shared, 0o640)
	assertMode(sharedDir, 0o750)
	fresh := filepath.Join(sharedDir, "fresh.mbox")
	initialize(fresh)
	assertMode(fresh, 0o600)
}
