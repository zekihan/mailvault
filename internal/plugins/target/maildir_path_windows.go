package target

import (
	"os"
	"path/filepath"
	"strings"
)

func openAbsoluteMaildir(absolute string, create bool, created *[]maildirCreation) (*maildirDirectory, error) {
	volume := filepath.VolumeName(absolute) + string(filepath.Separator)
	root, err := os.OpenRoot(volume)
	if err != nil {
		return nil, err
	}
	directory, err := openedMaildirDirectory(root, volume, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	return directory.walk(strings.TrimPrefix(absolute, volume), create, created)
}
