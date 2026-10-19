package target

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func syncMaildirDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func (t *MaildirTarget) syncDirectory(path string) error {
	syncDir := t.syncDir
	if syncDir == nil {
		syncDir = syncMaildirDirectory
	}
	return syncDir(path)
}

// Persist the structure as well as individual message entries. Sync ancestors
// too, so a first backup cannot acknowledge a file under an unsynced new parent.
// Unsupported directory sync operations fail closed rather than claim durability.
func (t *MaildirTarget) syncStructure() error {
	root, err := filepath.Abs(t.path)
	if err != nil {
		return err
	}
	directories := []string{filepath.Join(root, "cur"), filepath.Join(root, "new"), filepath.Join(root, "tmp")}
	for path := root; ; path = filepath.Dir(path) {
		directories = append(directories, path)
		if filepath.Dir(path) == path {
			break
		}
	}
	for _, path := range directories {
		if err := t.syncDirectory(path); err != nil {
			return fmt.Errorf("sync maildir directory %q: %w", path, err)
		}
	}
	return nil
}

func (t *MaildirTarget) rollbackPublication(path, directory string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove unconfirmed publication: %w", err)
	}
	if err := t.syncDirectory(directory); err != nil {
		return fmt.Errorf("sync publication rollback: %w", err)
	}
	return nil
}
