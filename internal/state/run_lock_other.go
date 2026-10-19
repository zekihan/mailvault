//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package state

import (
	"errors"
	"os"
)

var errSyncLockUnsupported = errors.New("cross-process sync locking is unsupported on this platform")

func tryLockSyncFile(*os.File) (bool, error) { return false, errSyncLockUnsupported }
func unlockSyncFile(*os.File) error          { return errSyncLockUnsupported }
