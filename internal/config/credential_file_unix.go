//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package config

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openCredentialFile(path string) (*os.File, error) {
	// NOFOLLOW prevents final-component symlink swaps. NONBLOCK prevents an
	// attacker swapping in a FIFO from blocking before the descriptor is checked.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func validateCredentialFile(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && int64(stat.Uid) != int64(os.Geteuid())) {
		return errors.New("credential file must be owned by root or the effective user")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("credential file must not be group- or world-writable")
	}
	return nil
}
