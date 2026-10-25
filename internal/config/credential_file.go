package config

import (
	"errors"
	"fmt"
	"io"
)

// maxCredentialFileSize bounds both password and JSON credential references.
const maxCredentialFileSize = 64 * 1024

func readCredentialFile(path string) (data []byte, err error) {
	file, err := openCredentialFile(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			data = nil
			err = errors.Join(err, closeErr)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("credential must be a regular file")
	}
	if err := validateCredentialFile(info); err != nil {
		return nil, err
	}
	if info.Size() > maxCredentialFileSize {
		return nil, fmt.Errorf("credential file exceeds %d bytes", maxCredentialFileSize)
	}
	// The extra byte detects a file growing after Stat without an unbounded read.
	data, err = io.ReadAll(io.LimitReader(file, maxCredentialFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCredentialFileSize {
		return nil, fmt.Errorf("credential file exceeds %d bytes", maxCredentialFileSize)
	}
	return data, nil
}
