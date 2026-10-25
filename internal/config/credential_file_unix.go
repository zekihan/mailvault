//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package config

import (
	"fmt"
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
	if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
		return fmt.Errorf("credential file must be owned by root or the effective user")
	}
	if info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("credential file must not be group- or world-writable")
	}
	return nil
}
