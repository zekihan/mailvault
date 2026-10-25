package target

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const maildirReadFlags = os.O_RDONLY

// NT hard-link publication takes both parent directory handles. A replaced
// pathname cannot redirect either endpoint, and reparse points are not followed.
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
	name, err := windows.NewNTUnicodeString(oldName)
	if err != nil {
		return err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(oldDir.Fd()),
		ObjectName:    name,
		Attributes:    windows.OBJ_DONT_REPARSE,
	}
	var source windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&source, windows.SYNCHRONIZE|windows.FILE_WRITE_ATTRIBUTES,
		&attributes, &status, nil, 0, windows.FILE_SHARE_DELETE|windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_NON_DIRECTORY_FILE, 0, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(source) }()
	encoded, err := windows.UTF16FromString(newName)
	if err != nil {
		return err
	}
	var link struct {
		ReplaceIfExists byte
		RootDirectory   windows.Handle
		FileNameLength  uint32
		FileName        [260]uint16
	}
	if len(encoded) > len(link.FileName) {
		return windows.ERROR_FILENAME_EXCED_RANGE
	}
	link.RootDirectory = windows.Handle(newDir.Fd())
	link.FileNameLength = uint32((len(encoded) - 1) * 2)
	copy(link.FileName[:], encoded)
	// FILE_INFORMATION_CLASS FileLinkInformation is 11 (the x/sys constant
	// named FileLinkInformation currently denotes FileLinkInformationEx).
	return windows.NtSetInformationFile(source, &status, (*byte)(unsafe.Pointer(&link)), uint32(unsafe.Sizeof(link)), 11)
}

// FlushFileBuffers requires write access. Reopen the pinned directory itself
// with an empty NT name, rather than reopening its potentially replaced path.
func syncOpenedMaildirDirectory(directory *maildirDirectory) error {
	pinned, err := directory.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = pinned.Close() }()
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(pinned.Fd()),
		ObjectName:    &windows.NTUnicodeString{},
		Attributes:    windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE,
		&attributes, &status, nil, 0, windows.FILE_SHARE_DELETE|windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return err
	}
	return errors.Join(windows.FlushFileBuffers(handle), windows.CloseHandle(handle))
}
