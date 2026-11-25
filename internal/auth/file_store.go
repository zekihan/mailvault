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

const (
	maxTokenFileSize      = 1 << 20
	privateTokenFilePerms = 0o600
	privateTokenDirPerms  = 0o700
)

type fileState struct {
	Account string `json:"account"`
	State   string `json:"state"`
}

// FileStore stores one account's opaque OAuth state in a private JSON file.
// It is intended as an explicit headless alternative to the OS keyring.
type FileStore struct{ Path string }

func (store FileStore) Get(ctx context.Context, account string) (string, error) {
	if account == "" || store.Path == "" {
		return "", errors.New("OAuth file path and account are required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := resolveTokenPath(store.Path, false)
	if err != nil {
		return "", err
	}
	data, err := readTokenFile(path)
	if err != nil {
		return "", err
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

func resolveTokenPath(path string, create bool) (string, error) {
	dir, err := privateTokenDir(filepath.Dir(path), create)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(path)), nil
}

func readTokenFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := checkTokenFileInfo(info); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if err := checkTokenFileSame(info, file); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTokenFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxTokenFileSize {
		return nil, errors.New("OAuth token file is too large")
	}
	return data, nil
}

func checkTokenFileInfo(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("OAuth token file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("OAuth token file permissions must be 0600 or stricter")
	}
	if info.Size() > maxTokenFileSize {
		return errors.New("OAuth token file is too large")
	}
	return nil
}

func checkTokenFileSame(info os.FileInfo, file *os.File) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return errors.New("OAuth token file changed while opening")
	}
	return nil
}

func (store FileStore) Set(ctx context.Context, account, value string) error {
	if account == "" || value == "" || store.Path == "" {
		return errors.New("OAuth file path, account, and state are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := resolveTokenPath(store.Path, true)
	if err != nil {
		return err
	}
	if err := store.checkExisting(ctx, account, path); err != nil {
		return err
	}
	tmp, err := writeTokenTempFile(path, account, value)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace OAuth token file: %w", err)
	}
	directory, err := os.Open(filepath.Dir(path))
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

func (store FileStore) checkExisting(ctx context.Context, account, path string) error {
	if _, err := os.Lstat(path); err == nil {
		if _, err := store.withPath(path).Get(ctx, account); err != nil {
			return fmt.Errorf("check existing OAuth token file: %w", err)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect existing OAuth token file: %w", err)
	}
	return nil
}

func writeTokenTempFile(path, account, value string) (string, error) {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".mailvault-oauth-*")
	if err != nil {
		return "", fmt.Errorf("create temporary OAuth token file: %w", err)
	}
	if err := writeTokenTemp(file, account, value); err != nil {
		return "", err
	}
	return file.Name(), nil
}

func writeTokenTemp(file *os.File, account, value string) error {
	tmp := file.Name()
	fail := func(err error) error {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Chmod(privateTokenFilePerms); err != nil {
		return fail(fmt.Errorf("secure OAuth token file: %w", err))
	}
	encoded, err := json.Marshal(fileState{Account: account, State: value})
	if err != nil {
		return fail(fmt.Errorf("encode OAuth token file: %w", err))
	}
	if _, err := file.Write(encoded); err != nil {
		return fail(fmt.Errorf("write OAuth token file: %w", err))
	}
	if err := file.Sync(); err != nil {
		return fail(fmt.Errorf("sync OAuth token file: %w", err))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close OAuth token file: %w", err)
	}
	return nil
}

func (store FileStore) withPath(path string) FileStore { store.Path = path; return store }

func privateTokenDir(path string, create bool) (string, error) {
	if create {
		if err := os.MkdirAll(path, privateTokenDirPerms); err != nil {
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
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("OAuth token directory must not be group or world writable")
	}
	return dir, nil
}
