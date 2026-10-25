package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsCredentialPathRejectsDevices(t *testing.T) {
	for _, path := range []string{
		"NUL", "CON", "COM1", "LPT1", "CONIN$", "CONOUT$",
		`C:\secrets\NUL`, `C:\secrets\COM1 \password`,
		`C:\secrets\password:stream`, `\\.\NUL`, `\\.\PhysicalDrive0`,
		`\\.\pipe\mailvault`, `\\?\C:\secrets\password`,
		`\\?\GLOBALROOT\Device\NamedPipe\mailvault`, `\\localhost\pipe\mailvault`,
	} {
		t.Run(path, func(t *testing.T) {
			if _, err := credentialWindowsPath(path); err == nil {
				t.Fatal("unsafe path accepted")
			}
			if value, err := readCredentialFile(path); err == nil || value != nil {
				t.Fatal("unsafe reference accepted")
			}
		})
	}
	for _, path := range []string{`C:\secrets\password`, `\\server\share\password`, `relative\password`} {
		if _, err := credentialWindowsPath(path); err != nil {
			t.Fatalf("ordinary filesystem path rejected: %v", err)
		}
	}
}

func TestWindowsCredentialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential")
	const secret = "synthetic-fixture\r\n"
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readCredentialFile(path); err != nil || string(got) != secret {
		t.Fatalf("regular credential not preserved: %v", err)
	}
	if value, err := readCredentialFile(dir); err == nil || value != nil {
		t.Fatal("directory accepted")
	}
	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(path, link); err != nil {
			if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
				t.Skip("Windows account lacks symlink privilege")
			}
			t.Fatal(err)
		}
		if value, err := readCredentialFile(link); err == nil || value != nil {
			t.Fatal("symlink accepted")
		}
	})
	for _, size := range []int64{maxCredentialFileSize, maxCredentialFileSize + 1} {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(size); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := readCredentialFile(path)
		if size == maxCredentialFileSize {
			if err != nil || len(got) != maxCredentialFileSize {
				t.Fatalf("limit-sized file rejected: %v", err)
			}
		} else if err == nil || got != nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized credential accepted: %v", err)
		}
	}
}
