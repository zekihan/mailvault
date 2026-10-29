//go:build unix

package target

import "golang.org/x/sys/unix"

const mboxNonblockFlag = unix.O_NONBLOCK
