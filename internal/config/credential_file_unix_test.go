//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package config

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCredentialFileRejectsUnsafeReferences(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("synthetic-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "symlink")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	socketDir := t.TempDir()
	socket := filepath.Join(socketDir, "socket")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for name, path := range map[string]string{"symlink": symlink, "fifo": fifo, "socket": socket, "device": "/dev/null", "directory": dir} {
		t.Run(name, func(t *testing.T) {
			value, err := resolveCredentialRef("file:" + path)
			if err == nil || value != "" {
				t.Fatalf("unsafe reference returned value or no error: %v", err)
			}
			if strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("error exposed credential")
			}
		})
	}
}

func TestCredentialFileSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	for _, size := range []int64{maxCredentialFileSize, maxCredentialFileSize + 1, 1 << 30} {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(size); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		value, err := resolveCredentialRef("file:" + path)
		if size == maxCredentialFileSize {
			if err != nil || len(value) != maxCredentialFileSize {
				t.Fatalf("limit-sized file: length %d, error %v", len(value), err)
			}
		} else if err == nil || value != "" || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized file (%d) was not rejected: %v", size, err)
		}
	}
}

func TestCredentialFileWritePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("synthetic-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o600, 0o644, 0o660, 0o602} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		value, err := resolveCredentialRef("file:" + path)
		if mode&0o022 != 0 {
			if err == nil || value != "" {
				t.Fatalf("writable mode %o accepted", mode)
			}
		} else if err != nil || value != "synthetic-secret\n" {
			t.Fatalf("safe mode %o failed: %v", mode, err)
		}
	}
}

type credentialOwnerInfo struct {
	os.FileInfo

	uid int
}

func (i credentialOwnerInfo) Sys() any { return &syscall.Stat_t{Uid: uint32(i.uid)} } // #nosec G115 -- test mock; UID fixtures are small ints, equality semantics preserved
func TestCredentialFileOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	current := os.Geteuid()
	other := current + 1
	if other <= 0 {
		other = 1
	}
	for _, uid := range []int{0, current, other} {
		err := validateCredentialFile(credentialOwnerInfo{FileInfo: info, uid: uid})
		if uid == other {
			if err == nil {
				t.Fatal("unrelated owner accepted")
			}
		} else if err != nil {
			t.Fatalf("trusted owner rejected: %v", err)
		}
	}
}
