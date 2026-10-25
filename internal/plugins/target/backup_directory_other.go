//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package target

import (
	"fmt"
	"os"
)

func openBackupDirectory(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("backup path %q is not a directory", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err = file.Stat()
	if err != nil || !info.IsDir() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("backup path %q is not a directory", path)
	}
	return file, nil
}
