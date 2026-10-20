//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package target

import (
	"errors"
	"os"
)

var errMboxLockUnsupported = errors.New("safe mbox file locking is unsupported on this platform")

func tryLockMbox(*os.File) (bool, error) { return false, errMboxLockUnsupported }
func unlockMbox(*os.File) error          { return errMboxLockUnsupported }
