//go:build !windows

package target

import "errors"

func syncOpenedMaildirDirectory(directory *maildirDirectory) error {
	file, err := directory.root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}
