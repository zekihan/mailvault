package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	syncFileLockRetryInterval = 10 * time.Millisecond
	syncFilePerms             = 0o600
)

var errReadOnlySyncLock = errors.New("read-only state cannot acquire a writable sync lock")
var errSyncLockNotRegular = errors.New("sync lock must be a regular file")

// LockSync serializes writers sharing this database across engine instances and
// processes. Keep the sidecar inode after release: unlinking it permits split locks.
// Dry runs must not call this, since acquiring a file-backed lock may create it.
func (store *Store) LockSync(ctx context.Context) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store.readOnly {
		return nil, errReadOnlySyncLock
	}
	if store.path == inMemoryDBPath {
		return store.lockInMemory(ctx)
	}
	file, err := openSyncLockFile(store.path)
	if err != nil {
		return nil, err
	}
	if err := waitSyncLock(ctx, file); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, unlockSyncFile(file), file.Close())
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() { releaseErr = errors.Join(unlockSyncFile(file), file.Close()) })
		return releaseErr
	}, nil
}

func (store *Store) lockInMemory(ctx context.Context) (func() error, error) {
	store.runGateOnce.Do(func() { store.runGate = make(chan struct{}, 1) })
	select {
	case store.runGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-store.runGate
		return nil, err
	}
	var once sync.Once
	return func() error { once.Do(func() { <-store.runGate }); return nil }, nil
}

func openSyncLockFile(path string) (*os.File, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve state lock path: %w", err)
	}
	file, err := os.OpenFile(canonical+".sync.lock", os.O_CREATE|os.O_RDWR, syncFilePerms)
	if err != nil {
		return nil, fmt.Errorf("open state sync lock: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(errSyncLockNotRegular, file.Close())
	}
	return file, nil
}

func waitSyncLock(ctx context.Context, file *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, file.Close())
		}
		locked, err := tryLockSyncFile(file)
		if err != nil {
			return errors.Join(err, file.Close())
		}
		if locked {
			return nil
		}
		if err := waitSyncLockRetry(ctx); err != nil {
			return errors.Join(err, file.Close())
		}
	}
}

func waitSyncLockRetry(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(syncFileLockRetryInterval):
		return nil
	}
}
