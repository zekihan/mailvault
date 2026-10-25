package target

import (
	"errors"
	"fmt"
	"os"
	"sort"
)

type maildirCreation struct {
	parent    *maildirDirectory
	directory *maildirDirectory
	name      string
}

func syncMaildirDirectory(path string) error {
	directory, err := openMaildirPath(path, false, nil)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return syncOpenedMaildirDirectory(directory)
}

func syncOpenedMaildirDirectory(directory *maildirDirectory) error {
	file, err := directory.root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func (t *MaildirTarget) syncOpenedDirectory(directory *maildirDirectory) error {
	if t.syncDir != nil {
		if err := t.syncDir(directory.path); err != nil {
			return err
		}
	}
	return syncOpenedMaildirDirectory(directory)
}

func closeMaildirCreations(created []maildirCreation) {
	for _, item := range created {
		_ = item.parent.Close()
		_ = item.directory.Close()
	}
}

func rollbackMaildirStructure(created []maildirCreation) error {
	var result error
	for i := len(created) - 1; i >= 0; i-- {
		item := created[i]
		info, err := item.parent.root.Lstat(item.name)
		if err == nil && !os.SameFile(info, item.directory.info) {
			err = fmt.Errorf("newly created directory identity changed: %s", item.directory.path)
		}
		if err == nil {
			err = item.parent.root.Remove(item.name)
		}
		if err != nil {
			result = errors.Join(result, fmt.Errorf("remove newly created maildir directory: %w", err))
		}
	}
	return result
}

// Sync only newly created directories and their parents, children first. All
// descriptors were acquired during creation; renamed paths are never reopened.
func (t *MaildirTarget) syncStructure(created []maildirCreation) error {
	needed := map[string]*maildirDirectory{}
	for _, item := range created {
		needed[item.directory.path] = item.directory
		needed[item.parent.path] = item.parent
	}
	ordered := make([]*maildirDirectory, 0, len(needed))
	for _, directory := range needed {
		ordered = append(ordered, directory)
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i].path) > len(ordered[j].path) })
	for _, directory := range ordered {
		if err := t.syncOpenedDirectory(directory); err != nil {
			return fmt.Errorf("sync maildir directory %q: %w", directory.path, err)
		}
	}
	return nil
}

func (t *MaildirTarget) rollbackPublication(directory *maildirDirectory, name string) error {
	if err := directory.root.Remove(name); err != nil {
		return fmt.Errorf("remove unconfirmed publication: %w", err)
	}
	if err := t.syncOpenedDirectory(directory); err != nil {
		return fmt.Errorf("sync publication rollback: %w", err)
	}
	return nil
}
