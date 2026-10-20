package target

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// mkdirMaildirPath creates each missing path component and returns exactly the
// directories created by this call, in parent-to-child order.
func mkdirMaildirPath(path string) ([]string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	missing := []string{}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return nil, fmt.Errorf("%q is not a directory", current)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, current)
		if filepath.Dir(current) == current {
			return nil, err
		}
	}
	created := []string{}
	for i := len(missing) - 1; i >= 0; i-- {
		err := os.Mkdir(missing[i], 0750)
		if err == nil {
			created = append(created, missing[i])
			continue
		}
		if errors.Is(err, os.ErrExist) {
			info, statErr := os.Stat(missing[i])
			if statErr == nil && info.IsDir() {
				continue
			}
			if statErr != nil {
				return created, statErr
			}
			return created, fmt.Errorf("%q is not a directory", missing[i])
		}
		return created, err
	}
	return created, nil
}

func removeOwnedMaildirDirs(dirs []string) error {
	var cleanup error
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Remove(dirs[i]); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanup = errors.Join(cleanup, fmt.Errorf("remove newly created maildir directory %q: %w", dirs[i], err))
		}
	}
	return cleanup
}

// Sync only directories created by Initialize and their immediate parents.
// Child directories are persisted bottom-up; unrelated ancestors are untouched.
func (t *MaildirTarget) syncStructure(created []string) error {
	needed := map[string]bool{}
	for _, dir := range created {
		needed[dir] = true
		needed[filepath.Dir(dir)] = true
	}
	ordered := make([]string, 0, len(needed))
	for path := range needed {
		ordered = append(ordered, path)
	}
	sort.Slice(ordered, func(i, j int) bool {
		// Every ancestor is a shorter prefix, so children precede parents.
		return len(ordered[i]) > len(ordered[j])
	})
	for _, path := range ordered {
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
