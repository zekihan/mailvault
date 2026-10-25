//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package target

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestBackupDirectoryRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		file, err := openBackupDirectory(path)
		if file != nil {
			_ = file.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO opened as a backup directory")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("directory open blocked on FIFO")
	}
}
