package target

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zekihan/mailvault/internal/plugins"
)

const (
	maildirDirPerms  = 0o750
	maildirFilePerms = 0o600
)

// A directory handle pins the inode used for every operation. Paths are only
// used to validate identity and to report errors, never to create/read mail.
type maildirDirectory struct {
	root *os.Root
	path string
	info os.FileInfo
}

func (dir *maildirDirectory) Close() error { return dir.root.Close() }

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

func (dir *maildirDirectory) child(name string, create bool, created *[]maildirCreation) (*maildirDirectory, error) {
	// Lstat and the identity comparison also reject symlinks on platforms where
	// Root.OpenRoot follows an in-root symlink. OpenRoot itself enforces containment.
	owned := false
	info, err := dir.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && create {
		err = dir.root.Mkdir(name, maildirDirPerms)
		owned = err == nil
		if err == nil || errors.Is(err, os.ErrExist) {
			info, err = dir.root.Lstat(name)
		}
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("maildir component %q is not a real directory", filepath.Join(dir.path, name))
	}
	root, err := dir.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	child, err := openedMaildirDirectory(root, filepath.Join(dir.path, name), info)
	if err != nil {
		return nil, err
	}
	if owned && created != nil {
		parentCopy, copyErr := dir.walk(".", false, nil)
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

func (dir *maildirDirectory) walk(path string, create bool, created *[]maildirCreation) (*maildirDirectory, error) {
	clean := filepath.Clean(path)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, errors.New("invalid relative maildir path")
	}
	root, err := dir.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	current, err := openedMaildirDirectory(root, dir.path, dir.info)
	if err != nil {
		return nil, err
	}
	if clean == "." {
		return current, nil
	}
	for part := range strings.SplitSeq(clean, string(filepath.Separator)) {
		next, err := current.child(part, create, created)
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

func (dir *maildirDirectory) validate() error {
	current, err := openMaildirPath(dir.path, false, nil)
	if err != nil {
		return fmt.Errorf("revalidate maildir: %w", err)
	}
	defer func() { _ = current.Close() }()
	if !os.SameFile(dir.info, current.info) {
		return fmt.Errorf("maildir directory identity changed: %s", dir.path)
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

func (dir *maildirDirectory) createTemp() (*os.File, string, error) {
	for range 100 {
		name := "mailvault-" + rand.Text()
		file, err := dir.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, maildirFilePerms)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return file, name, err
	}
	return nil, "", errors.New("could not reserve a unique maildir filename")
}

func removeMaildirFile(dir *maildirDirectory, name string) error { return dir.root.Remove(name) }

func (dir *maildirDirectory) openMessage(name string) (*os.File, error) {
	info, err := dir.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("maildir locator is not a regular file")
	}
	file, err := dir.root.OpenFile(name, maildirReadFlags, 0)
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
	if err := plugins.CheckMessageSize(info.Size(), limit); err != nil {
		return nil, err
	}
	return plugins.ReadMessage(file, limit)
}

func (dir *maildirDirectory) walkEntries(ctx context.Context, budget *backupListingBudget, prefix string, visit func(os.DirEntry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dir.validate(); err != nil {
		return err
	}
	file, err := dir.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := file.ReadDir(backupDirectoryBatchSize)
		for _, entry := range entries {
			if err := dir.visitEntry(ctx, budget, prefix, entry, visit); err != nil {
				return err
			}
		}
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if readErr == io.EOF {
			if err := ctx.Err(); err != nil {
				return err
			}
			return dir.validate()
		}
	}
}

func (dir *maildirDirectory) visitEntry(ctx context.Context, budget *backupListingBudget, prefix string, entry os.DirEntry, visit func(os.DirEntry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dir.validate(); err != nil {
		return err
	}
	if err := budget.add(filepath.ToSlash(filepath.Join(prefix, entry.Name()))); err != nil {
		return err
	}
	if err := visit(entry); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return dir.validate()
}

func (dir *maildirDirectory) resolveMessage(locator string) (*os.File, string, error) {
	return dir.resolveMessageBounded(context.Background(), locator, backupListingLimits{})
}

func (dir *maildirDirectory) resolveMessageBounded(ctx context.Context, locator string, limits backupListingLimits) (*os.File, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	clean, err := cleanMaildirLocator(locator)
	if err != nil {
		return nil, "", err
	}
	parent := filepath.Dir(clean)
	file, err := dir.openExactMessage(ctx, clean, parent)
	if err == nil {
		return file, clean, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	return dir.findRenamedMessage(ctx, locator, clean, parent, limits)
}

func cleanMaildirLocator(locator string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(locator))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid maildir locator")
	}
	return clean, nil
}

func (dir *maildirDirectory) openExactMessage(ctx context.Context, clean, parent string) (*os.File, error) {
	directory, err := dir.walk(parent, false, nil)
	if err != nil {
		return nil, err
	}
	file, openErr := directory.openMessage(filepath.Base(clean))
	if openErr == nil {
		openErr = validateMaildirDirectories(dir, directory)
	}
	if openErr == nil {
		openErr = ctx.Err()
	}
	_ = directory.Close()
	if openErr == nil {
		return file, nil
	}
	if file != nil {
		_ = file.Close()
	}
	return nil, openErr
}

func (dir *maildirDirectory) findRenamedMessage(ctx context.Context, locator, clean, parent string, limits backupListingLimits) (*os.File, string, error) {
	base, _, _ := strings.Cut(filepath.Base(clean), ":2,")
	folder := filepath.Dir(parent)
	search := &maildirRenameSearch{dir: dir, locator: locator, base: base, folder: folder, budget: limits.budget()}
	for _, subdir := range []string{maildirDirCur, maildirDirNew} {
		if err := search.searchSubdir(ctx, subdir); err != nil {
			search.closeFound()
			return nil, "", err
		}
	}
	if err := ctx.Err(); err != nil {
		search.closeFound()
		return nil, "", err
	}
	if search.found == nil {
		return nil, "", fmt.Errorf("%w: %s", plugins.ErrBackupMessageMissing, locator)
	}
	if err := dir.validateMessage(search.relative, search.found); err != nil {
		_ = search.found.Close()
		return nil, "", err
	}
	if err := dir.validate(); err != nil {
		_ = search.found.Close()
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		_ = search.found.Close()
		return nil, "", err
	}
	return search.found, search.relative, nil
}

type maildirRenameSearch struct {
	dir      *maildirDirectory
	locator  string
	base     string
	folder   string
	budget   *backupListingBudget
	found    *os.File
	relative string
}

func (search *maildirRenameSearch) closeFound() {
	if search.found != nil {
		_ = search.found.Close()
	}
}

func (search *maildirRenameSearch) searchSubdir(ctx context.Context, subdir string) error {
	directory, err := search.dir.walk(filepath.Join(search.folder, subdir), false, nil)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.walkEntries(ctx, search.budget, filepath.Join(search.folder, subdir), func(entry os.DirEntry) error {
		return search.visitEntry(directory, subdir, entry)
	})
}

func (search *maildirRenameSearch) visitEntry(directory *maildirDirectory, subdir string, entry os.DirEntry) error {
	if !entry.Type().IsRegular() || (entry.Name() != search.base && !strings.HasPrefix(entry.Name(), search.base+":2,")) {
		return nil
	}
	file, err := directory.openMessage(entry.Name())
	if err != nil {
		return err
	}
	if search.found != nil {
		_ = file.Close()
		return fmt.Errorf("maildir locator %q has multiple matching files", search.locator)
	}
	search.found = file
	search.relative = filepath.Join(search.folder, subdir, entry.Name())
	return nil
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

func (dir *maildirDirectory) validateMessage(relative string, file *os.File) error {
	directory, err := dir.walk(filepath.Dir(relative), false, nil)
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
