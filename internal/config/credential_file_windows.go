package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func credentialWindowsPath(path string) (string, error) {
	// Exclude Win32/NT device namespaces before opening anything. Ordinary drive
	// and UNC filesystem paths remain supported, including relative references.
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	volume := filepath.VolumeName(path)
	if strings.HasPrefix(volume, `\\`) {
		parts := strings.Split(strings.TrimPrefix(volume, `\\`), `\`)
		if len(parts) != 2 || parts[0] == "." || parts[0] == "?" || strings.EqualFold(parts[1], "pipe") {
			return "", fmt.Errorf("credential path must name a filesystem file")
		}
	} else if len(volume) != 2 || volume[1] != ':' || (volume[0] < 'A' || volume[0] > 'Z') && (volume[0] < 'a' || volume[0] > 'z') {
		return "", fmt.Errorf("credential path must name a filesystem file")
	}
	relative := strings.TrimLeft(path[len(volume):], `\/`)
	// IsLocal rejects reserved DOS devices in every component and colons (ADS).
	if !filepath.IsLocal(relative) {
		return "", fmt.Errorf("credential path must name a filesystem file")
	}
	return path, nil
}

func openCredentialFile(path string) (*os.File, error) {
	path, err := credentialWindowsPath(path)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// Open the reparse point itself, never its target. Share only for reads so a
	// writer cannot replace or change the opened file while it is being checked.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	reject := func(err error) (*os.File, error) {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	kind, err := windows.GetFileType(handle)
	if err != nil {
		return reject(err)
	}
	if kind != windows.FILE_TYPE_DISK {
		return reject(fmt.Errorf("credential must be a regular disk file"))
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return reject(err)
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_DEVICE) != 0 {
		return reject(fmt.Errorf("credential must be a regular file without reparse points"))
	}
	return os.NewFile(uintptr(handle), path), nil
}

func validateCredentialFile(os.FileInfo) error {
	// POSIX ownership/mode bits do not describe Windows DACLs. As before, the
	// operator controls the credential's ACL and trusted parent directories.
	return nil
}
