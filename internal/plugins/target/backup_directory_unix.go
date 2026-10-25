//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package target

import (
	"os"
	"syscall"
)

// Preserve ReadDir(path)'s directory-only open while reading bounded batches.
// NOFOLLOW also preserves WalkDir's refusal to descend into a symlink entry.
func openBackupDirectory(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
}
