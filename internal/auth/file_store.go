package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxTokenFileSize = 1 << 20

type fileState struct {
	Account string `json:"account"`
	State   string `json:"state"`
}

// FileStore stores one account's opaque OAuth state in a private JSON file.
// It is intended as an explicit headless alternative to the OS keyring.
type FileStore struct{ Path string }

func (s FileStore) Get(ctx context.Context, account string) (string, error) {
	if account == "" || s.Path == "" {
		return "", errors.New("OAuth file path and account are required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir, err := privateTokenDir(filepath.Dir(s.Path), false)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, filepath.Base(s.Path))
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("OAuth token file must be a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return "", errors.New("OAuth token file permissions must be 0600 or stricter")
	}
	if info.Size() > maxTokenFileSize {
		return "", errors.New("OAuth token file is too large")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return "", errors.New("OAuth token file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTokenFileSize+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxTokenFileSize {
		return "", errors.New("OAuth token file is too large")
	}
	var saved fileState
	if err := json.Unmarshal(data, &saved); err != nil || saved.Account == "" || saved.State == "" {
		return "", errors.New("OAuth token file is invalid")
	}
	if saved.Account != account {
		return "", errors.New("OAuth token file belongs to a different account")
	}
	return saved.State, nil
}

func (s FileStore) Set(ctx context.Context, account, value string) error {
	if account == "" || value == "" || s.Path == "" {
		return errors.New("OAuth file path, account, and state are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := privateTokenDir(filepath.Dir(s.Path), true)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, filepath.Base(s.Path))
	if _, err := os.Lstat(path); err == nil {
		if _, err := s.withPath(path).Get(ctx, account); err != nil {
			return fmt.Errorf("check existing OAuth token file: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect existing OAuth token file: %w", err)
	}
	file, err := os.CreateTemp(dir, ".mailvault-oauth-*")
	if err != nil {
		return fmt.Errorf("create temporary OAuth token file: %w", err)
	}
	tmp := file.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure OAuth token file: %w", err)
	}
	encoded, err := json.Marshal(fileState{Account: account, State: value})
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("encode OAuth token file: %w", err)
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return fmt.Errorf("write OAuth token file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync OAuth token file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close OAuth token file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace OAuth token file: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open OAuth token directory: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return fmt.Errorf("sync OAuth token directory: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close OAuth token directory: %w", closeErr)
	}
	return nil
}

func (s FileStore) withPath(path string) FileStore { s.Path = path; return s }

func privateTokenDir(path string, create bool) (string, error) {
	if create {
		if err := os.MkdirAll(path, 0700); err != nil {
			return "", fmt.Errorf("create OAuth token directory: %w", err)
		}
	}
	dir, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve OAuth token directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("inspect OAuth token directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return "", errors.New("OAuth token directory must not be group or world writable")
	}
	return dir, nil
}
