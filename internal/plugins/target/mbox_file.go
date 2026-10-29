package target

import (
	"errors"
	"os"
)

// Open without waiting for a FIFO peer, then validate the same descriptor that
// callers will use. Checking the pathname before opening would leave a race.
func openMboxFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	file, err := os.OpenFile(path, flags|mboxNonblockFlag, mode)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("mbox must be a regular file")
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
