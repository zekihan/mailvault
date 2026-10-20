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

var errReadOnlySyncLock = errors.New("read-only state cannot acquire a writable sync lock")
var errSyncLockNotRegular = errors.New("sync lock must be a regular file")

// LockSync serializes writers sharing this database across engine instances and
// processes. Keep the sidecar inode after release: unlinking it permits split locks.
// Dry runs must not call this, since acquiring a file-backed lock may create it.
func (s *Store) LockSync(ctx context.Context) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.readOnly {
		return nil, errReadOnlySyncLock
	}
	if s.path == ":memory:" {
		s.runGateOnce.Do(func() { s.runGate = make(chan struct{}, 1) })
		select {
		case s.runGate <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			<-s.runGate
			return nil, err
		}
		var once sync.Once
		return func() error { once.Do(func() { <-s.runGate }); return nil }, nil
	}
	canonical, err := filepath.EvalSymlinks(s.path)
	if err != nil {
		return nil, fmt.Errorf("resolve state lock path: %w", err)
	}
	file, err := os.OpenFile(canonical+".sync.lock", os.O_CREATE|os.O_RDWR, 0600)
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
	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, file.Close())
		}
		locked, err := tryLockSyncFile(file)
		if err != nil {
			return nil, errors.Join(err, file.Close())
		}
		if locked {
			break
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), file.Close())
		case <-time.After(10 * time.Millisecond):
		}
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
