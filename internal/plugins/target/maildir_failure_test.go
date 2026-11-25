//go:build linux

package target

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMaildirPreservesWriteAndCleanupErrors(t *testing.T) {
	if os.Getenv("MAILVAULT_WRITE_FAILURE_CHILD") != "1" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMaildirPreservesWriteAndCleanupErrors$", "-test.v") // #nosec G204,G702 -- self-spawned test binary; argv fully controlled
		cmd.Env = append(os.Environ(), "MAILVAULT_WRITE_FAILURE_CHILD=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("write failure regression: %v\n%s", err, output)
		}
		return
	}
	path := physicalTempDir(t)
	target, err := NewMaildirTarget(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	maildir := mustAssert[*MaildirTarget](t, target)
	if err := maildir.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	maildir.removeTemp = func(*maildirDirectory, string) error { return os.ErrPermission }
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 256, Max: original.Max}); err != nil {
		t.Fatal(err)
	}
	writeErr := maildir.WriteMessage(t.Context(), &plugins.Message{Raw: []byte(strings.Repeat("x", 1500))})
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(writeErr, syscall.EFBIG) || !errors.Is(writeErr, os.ErrPermission) {
		t.Fatalf("did not retain write and cleanup errors: %v", writeErr)
	}
	files, err := os.ReadDir(filepath.Join(path, "new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatal("failed write published a partial message")
	}
}
