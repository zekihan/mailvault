//go:build unix

package target

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const maildirReadFlags = os.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK

func linkMaildirFile(from *maildirDirectory, oldName string, to *maildirDirectory, newName string) error {
	oldDir, err := from.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = oldDir.Close() }()
	newDir, err := to.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = newDir.Close() }()
	// flags=0 links the entry itself, never the target of a replaced symlink.
	if err := unix.Linkat(int(oldDir.Fd()), oldName, int(newDir.Fd()), newName, 0); err != nil {
		return err
	}
	info, err := to.root.Lstat(newName)
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("maildir publication is not a regular file")
	}
	if err != nil {
		return errors.Join(err, to.root.Remove(newName))
	}
	return nil
}

// Walk every configured-root component with no-follow directory descriptors.
// Search-only descriptors allow existing execute-only ancestors on Unix.
func openAbsoluteMaildir(path string, create bool, created *[]maildirCreation) (*maildirDirectory, error) {
	fd, err := unix.Open("/", maildirSearchFlags|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), "/")
	defer func() { _ = current.Close() }()
	prefix := "/"
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		nextFD, err := unix.Openat(int(current.Fd()), part, maildirSearchFlags|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		owned := false
		if errors.Is(err, unix.ENOENT) && create {
			err = unix.Mkdirat(int(current.Fd()), part, 0750)
			owned = err == nil
			if err == nil || errors.Is(err, unix.EEXIST) {
				nextFD, err = unix.Openat(int(current.Fd()), part, maildirSearchFlags|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
		}
		if err != nil {
			return nil, &os.PathError{Op: "open maildir component", Path: filepath.Join(prefix, part), Err: err}
		}
		next := os.NewFile(uintptr(nextFD), filepath.Join(prefix, part))
		if owned && created != nil {
			parent, err := rootForMaildirDescriptor(current, prefix)
			if err != nil {
				_ = next.Close()
				return nil, err
			}
			child, err := rootForMaildirDescriptor(next, filepath.Join(prefix, part))
			if err != nil {
				_ = parent.Close()
				_ = next.Close()
				return nil, err
			}
			*created = append(*created, maildirCreation{parent: parent, directory: child, name: part})
		}
		_ = current.Close()
		current = next
		prefix = filepath.Join(prefix, part)
	}
	return rootForMaildirDescriptor(current, path)
}

func rootForMaildirDescriptor(file *os.File, path string) (*maildirDirectory, error) {
	expected, err := file.Stat()
	if err != nil {
		return nil, err
	}
	// OpenRoot has no descriptor constructor. Compare the reopened directory to
	// the inode acquired by the no-follow walk before allowing any operations.
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return openedMaildirDirectory(root, path, expected)
}
