package target

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func tryLockMbox(file *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 0xffffffff, 0xffffffff, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}
func unlockMbox(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 0xffffffff, 0xffffffff, &windows.Overlapped{})
}
