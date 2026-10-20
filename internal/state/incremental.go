package state

import "context"

func (s *Store) KnownUIDs(ctx context.Context, source, target, scope, folder string) (map[string]bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='imap_receipts')`).Scan(&exists); err != nil {
		return nil, err
	}
	known := make(map[string]bool)
	if !exists {
		return known, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT uid FROM imap_receipts WHERE source=? AND target=? AND scope=? AND folder=?`, source, target, scope, folder)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		known[uid] = true
	}
	return known, rows.Err()
}

func (s *Store) MarkUIDDelivered(ctx context.Context, source, target, scope, folder, uid string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO imap_receipts(source,target,scope,folder,uid) VALUES(?,?,?,?,?)`, source, target, scope, folder, uid)
	return err
}
