//go:build unix && !linux && !darwin && !freebsd

package target

import "golang.org/x/sys/unix"

const maildirSearchFlags = unix.O_RDONLY
