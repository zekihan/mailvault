package config

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// maxCredentialFileSize bounds both password and JSON credential references.
const maxCredentialFileSize = 64 * 1024

func readCredentialFile(path string) ([]byte, error) {
	file, err := openCredentialFile(path)
	if err != nil {
		return nil, err
	}
	data, readErr := readCredentialData(file)
	if closeErr := file.Close(); closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	return data, readErr
}

func readCredentialData(file *os.File) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("credential must be a regular file")
	}
	if err := validateCredentialFile(info); err != nil {
		return nil, err
	}
	if info.Size() > maxCredentialFileSize {
		return nil, fmt.Errorf("credential file exceeds %d bytes", maxCredentialFileSize)
	}
	// The extra byte detects a file growing after Stat without an unbounded read.
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCredentialFileSize {
		return nil, fmt.Errorf("credential file exceeds %d bytes", maxCredentialFileSize)
	}
	return data, nil
}
