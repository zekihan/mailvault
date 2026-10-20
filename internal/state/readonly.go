package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// NewReadOnlyStore opens existing state without migrations or application writes.
// Legacy state without per-target delivery rows is conservatively treated as new.
func NewReadOnlyStore(path string) (*Store, error) {
	filename, err := stateFilename(path, false)
	if err != nil {
		return nil, err
	}
	dsn, err := stateDSN(filename, true)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only state: %w", err)
	}
	var exists bool
	if err := db.QueryRowContext(context.Background(), `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='deliveries')`).Scan(&exists); err != nil {
		return nil, errors.Join(fmt.Errorf("read state schema: %w", err), db.Close())
	}
	return &Store{db: db, path: filename, missingDeliveries: !exists, readOnly: true}, nil
}

// NewCleanupStore opens an existing state database for narrowly scoped
// maintenance without applying schema migrations.
func NewCleanupStore(ctx context.Context, path string) (*Store, error) {
	filename, err := stateFilename(path, false)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("state database must be a regular file")
	}
	dsn, err := stateDSN(filename, false)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open cleanup state: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("open cleanup state: %w", err), db.Close())
	}
	return &Store{db: db, path: filename}, nil
}
