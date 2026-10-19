package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
)

// NewReadOnlyStore opens existing state without migrations or application writes.
// Legacy state without per-target delivery rows is conservatively treated as new.
func NewReadOnlyStore(path string) (*Store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=ro&_query_only=1&_busy_timeout=5000"}
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, fmt.Errorf("open read-only state: %w", err)
	}
	var exists bool
	if err := db.QueryRowContext(context.Background(), `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='deliveries')`).Scan(&exists); err != nil {
		return nil, errors.Join(fmt.Errorf("read state schema: %w", err), db.Close())
	}
	return &Store{db: db, path: path, missingDeliveries: !exists}, nil
}
