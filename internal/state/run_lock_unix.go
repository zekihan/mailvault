//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package state

import (
	"errors"
	"os"
	"syscall"
)

func tryLockSyncFile(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func unlockSyncFile(file *os.File) error { return syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }
