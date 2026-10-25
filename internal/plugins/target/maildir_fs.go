package target

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zekihan/mailvault/internal/plugins"
)

// A directory handle pins the inode used for every operation. Paths are only
// used to validate identity and to report errors, never to create/read mail.
type maildirDirectory struct {
	root *os.Root
	path string
	info os.FileInfo
}

func (d *maildirDirectory) Close() error { return d.root.Close() }

func openedMaildirDirectory(root *os.Root, path string, expected os.FileInfo) (*maildirDirectory, error) {
	info, err := root.Stat(".")
	if err == nil && (!info.IsDir() || (expected != nil && !os.SameFile(expected, info))) {
		err = fmt.Errorf("maildir directory identity changed: %s", path)
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return &maildirDirectory{root: root, path: path, info: info}, nil
}

func openMaildirPath(path string, create bool, created *[]maildirCreation) (*maildirDirectory, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return openAbsoluteMaildir(absolute, create, created)
}

func (d *maildirDirectory) child(name string, create bool, created *[]maildirCreation) (*maildirDirectory, error) {
	// Lstat and the identity comparison also reject symlinks on platforms where
	// Root.OpenRoot follows an in-root symlink. OpenRoot itself enforces containment.
	owned := false
	info, err := d.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && create {
		err = d.root.Mkdir(name, 0750)
		owned = err == nil
		if err == nil || errors.Is(err, os.ErrExist) {
			info, err = d.root.Lstat(name)
		}
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("maildir component %q is not a real directory", filepath.Join(d.path, name))
	}
	root, err := d.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	child, err := openedMaildirDirectory(root, filepath.Join(d.path, name), info)
	if err != nil {
		return nil, err
	}
	if owned && created != nil {
		parentCopy, copyErr := d.walk(".", false, nil)
		if copyErr != nil {
			_ = child.Close()
			return nil, copyErr
		}
		childCopy, copyErr := child.walk(".", false, nil)
		if copyErr != nil {
			_ = parentCopy.Close()
			_ = child.Close()
			return nil, copyErr
		}
		*created = append(*created, maildirCreation{parent: parentCopy, directory: childCopy, name: name})
	}
	return child, nil
}

func (d *maildirDirectory) walk(path string, create bool, created *[]maildirCreation) (*maildirDirectory, error) {
	clean := filepath.Clean(path)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, errors.New("invalid relative maildir path")
	}
	root, err := d.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	current, err := openedMaildirDirectory(root, d.path, d.info)
	if err != nil {
		return nil, err
	}
	if clean == "." {
		return current, nil
	}
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		next, err := current.child(part, create, created)
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

func (d *maildirDirectory) validate() error {
	current, err := openMaildirPath(d.path, false, nil)
	if err != nil {
		return fmt.Errorf("revalidate maildir: %w", err)
	}
	defer func() { _ = current.Close() }()
	if !os.SameFile(d.info, current.info) {
		return fmt.Errorf("maildir directory identity changed: %s", d.path)
	}
	return nil
}

func validateMaildirDirectories(directories ...*maildirDirectory) error {
	for _, directory := range directories {
		if err := directory.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (d *maildirDirectory) createTemp() (*os.File, string, error) {
	for range 100 {
		name := "mailvault-" + rand.Text()
		file, err := d.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return file, name, err
	}
	return nil, "", errors.New("could not reserve a unique maildir filename")
}

func removeMaildirFile(d *maildirDirectory, name string) error { return d.root.Remove(name) }

// Open once, then check and read the same inode. O_NONBLOCK prevents a swapped
// FIFO from hanging before fstat; O_NOFOLLOW rejects a swapped final symlink.
func (d *maildirDirectory) openMessage(name string) (*os.File, error) {
	info, err := d.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("maildir locator is not a regular file")
	}
	file, err := d.root.OpenFile(name, maildirReadFlags, 0)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err == nil && (!opened.Mode().IsRegular() || !os.SameFile(info, opened)) {
		err = errors.New("maildir message identity changed")
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func readMaildirMessage(file *os.File, limit int64) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("maildir locator is not a regular file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("maildir message exceeds the %d byte verification limit", limit)
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(raw)) > limit {
		err = fmt.Errorf("maildir message exceeds the %d byte verification limit", limit)
	}
	return raw, err
}

func (d *maildirDirectory) entries() ([]os.DirEntry, error) {
	file, err := d.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return file.ReadDir(-1)
}

// resolveMessage preserves Maildir's new/cur moves and flag renames, returning
// the descriptor that was checked rather than a pathname to reopen later.
func (d *maildirDirectory) resolveMessage(locator string) (*os.File, string, error) {
	clean := filepath.Clean(filepath.FromSlash(locator))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, "", errors.New("invalid maildir locator")
	}
	parent := filepath.Dir(clean)
	directory, err := d.walk(parent, false, nil)
	if err == nil {
		file, openErr := directory.openMessage(filepath.Base(clean))
		_ = directory.Close()
		if openErr == nil {
			return file, clean, nil
		}
		err = openErr
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	base := strings.SplitN(filepath.Base(clean), ":2,", 2)[0]
	folder := filepath.Dir(parent)
	var found *os.File
	var relative string
	for _, subdir := range []string{"cur", "new"} {
		directory, err := d.walk(filepath.Join(folder, subdir), false, nil)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			if found != nil {
				_ = found.Close()
			}
			return nil, "", err
		}
		entries, err := directory.entries()
		if err == nil {
			for _, entry := range entries {
				if !entry.Type().IsRegular() || (entry.Name() != base && !strings.HasPrefix(entry.Name(), base+":2,")) {
					continue
				}
				file, openErr := directory.openMessage(entry.Name())
				if openErr != nil {
					err = openErr
					break
				}
				if found != nil {
					_ = file.Close()
					err = fmt.Errorf("maildir locator %q has multiple matching files", locator)
					break
				}
				found = file
				relative = filepath.Join(folder, subdir, entry.Name())
			}
		}
		_ = directory.Close()
		if err != nil {
			if found != nil {
				_ = found.Close()
			}
			return nil, "", err
		}
	}
	if found == nil {
		return nil, "", fmt.Errorf("%w: %s", plugins.ErrBackupMessageMissing, locator)
	}
	return found, relative, nil
}

func checkMaildirPublication(directory *maildirDirectory, name string, expected os.FileInfo) error {
	info, err := directory.root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(info, expected) {
		return errors.New("maildir publication identity changed")
	}
	return nil
}

func (d *maildirDirectory) validateMessage(relative string, file *os.File) error {
	directory, err := d.walk(filepath.Dir(relative), false, nil)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	expected, err := file.Stat()
	if err != nil {
		return err
	}
	return checkMaildirPublication(directory, filepath.Base(relative), expected)
}
