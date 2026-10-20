package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/zekihan/mailvault/internal/plugins"
)

func (s *Store) RecordBackupMessage(ctx context.Context, item plugins.BackupMessage) error {
	flags, err := json.Marshal(item.Flags)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO backup_messages(source_name,target_name,scope,folder,uid,locator,raw_sha256,flags_json,internal_date) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(source_name,target_name,scope,folder,uid) DO UPDATE SET locator=excluded.locator,raw_sha256=excluded.raw_sha256,flags_json=excluded.flags_json,internal_date=excluded.internal_date,recorded_at=CURRENT_TIMESTAMP`,
		item.Source, item.Target, item.Scope, item.Folder, item.UID, item.Locator, item.RawSHA256, string(flags), item.InternalDate)
	return err
}

func (s *Store) BackupMessages(ctx context.Context, target string) ([]plugins.BackupMessage, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='backup_messages')`).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT source_name,target_name,scope,folder,uid,locator,raw_sha256,flags_json,internal_date FROM backup_messages WHERE target_name=? ORDER BY source_name,scope,folder,uid`, target)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var items []plugins.BackupMessage
	for rows.Next() {
		var item plugins.BackupMessage
		var flags string
		if err := rows.Scan(&item.Source, &item.Target, &item.Scope, &item.Folder, &item.UID, &item.Locator, &item.RawSHA256, &flags, &item.InternalDate); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(flags), &item.Flags); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) FindBackupMessage(ctx context.Context, source, target, scope, folder, uid string) (plugins.BackupMessage, bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='backup_messages')`).Scan(&exists); err != nil {
		return plugins.BackupMessage{}, false, err
	}
	if !exists {
		return plugins.BackupMessage{}, false, nil
	}
	var item plugins.BackupMessage
	var flags string
	err := s.db.QueryRowContext(ctx, `SELECT source_name,target_name,scope,folder,uid,locator,raw_sha256,flags_json,internal_date FROM backup_messages WHERE source_name=? AND target_name=? AND scope=? AND folder=? AND uid=?`, source, target, scope, folder, uid).
		Scan(&item.Source, &item.Target, &item.Scope, &item.Folder, &item.UID, &item.Locator, &item.RawSHA256, &flags, &item.InternalDate)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return plugins.BackupMessage{}, false, nil
		}
		return plugins.BackupMessage{}, false, err
	}
	if err := json.Unmarshal([]byte(flags), &item.Flags); err != nil {
		return plugins.BackupMessage{}, false, err
	}
	return item, true, nil
}

func (s *Store) FindBackupByHash(ctx context.Context, target, rawSHA256 string) (plugins.BackupMessage, bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='backup_messages')`).Scan(&exists); err != nil {
		return plugins.BackupMessage{}, false, err
	}
	if !exists {
		return plugins.BackupMessage{}, false, nil
	}
	var item plugins.BackupMessage
	var flags string
	err := s.db.QueryRowContext(ctx, `SELECT source_name,target_name,scope,folder,uid,locator,raw_sha256,flags_json,internal_date FROM backup_messages WHERE target_name=? AND raw_sha256=? ORDER BY recorded_at LIMIT 1`, target, rawSHA256).Scan(&item.Source, &item.Target, &item.Scope, &item.Folder, &item.UID, &item.Locator, &item.RawSHA256, &flags, &item.InternalDate)
	if errors.Is(err, sql.ErrNoRows) {
		return plugins.BackupMessage{}, false, nil
	}
	if err != nil {
		return plugins.BackupMessage{}, false, err
	}
	if err := json.Unmarshal([]byte(flags), &item.Flags); err != nil {
		return plugins.BackupMessage{}, false, err
	}
	return item, true, nil
}

func (s *Store) LegacyBackupCandidates(ctx context.Context, target string) ([]plugins.BackupMessage, error) {
	var deliveries, messages bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='deliveries')`).Scan(&deliveries); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='messages')`).Scan(&messages); err != nil {
		return nil, err
	}
	if !deliveries {
		return nil, nil
	}
	identityHash := `CASE WHEN d.identity LIKE 'hash:%' THEN substr(d.identity,6) ELSE '' END`
	hashExpression := identityHash
	joinMessages := ""
	if messages {
		joinMessages = `LEFT JOIN messages m ON m.source_name=d.source_name AND m.folder=d.folder AND m.uid=d.uid`
		hashExpression = `COALESCE(m.content_hash, ` + identityHash + `)`
	}
	excludeManifest := ""
	var manifestExists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='backup_messages')`).Scan(&manifestExists); err != nil {
		return nil, err
	}
	if manifestExists {
		excludeManifest = `AND NOT EXISTS (
			SELECT 1 FROM backup_messages b
			WHERE b.source_name=d.source_name AND b.target_name=d.target_name AND b.folder=d.folder
			AND (b.uid=d.uid OR (d.uid='' AND b.raw_sha256=` + hashExpression + `))
		)`
	}
	query := `SELECT d.source_name,d.target_name,d.folder,d.uid,` + hashExpression + ` FROM deliveries d ` + joinMessages + ` WHERE d.target_name=? ` + excludeManifest
	rows, err := s.db.QueryContext(ctx, query, target)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []plugins.BackupMessage
	for rows.Next() {
		var item plugins.BackupMessage
		if err := rows.Scan(&item.Source, &item.Target, &item.Folder, &item.UID, &item.RawSHA256); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) MarkBackupObjectSuperseded(ctx context.Context, target, locator, replacement string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO superseded_backup_objects(target_name,locator,replacement_locator) VALUES(?,?,?) ON CONFLICT(target_name,locator) DO UPDATE SET replacement_locator=excluded.replacement_locator,recorded_at=CURRENT_TIMESTAMP`, target, locator, replacement)
	return err
}

func (s *Store) SupersededBackupObjects(ctx context.Context, target string) (map[string]bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='superseded_backup_objects')`).Scan(&exists); err != nil {
		return nil, err
	}
	result := make(map[string]bool)
	if !exists {
		return result, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT locator FROM superseded_backup_objects WHERE target_name=?`, target)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var locator string
		if err := rows.Scan(&locator); err != nil {
			return nil, err
		}
		result[locator] = true
	}
	return result, rows.Err()
}
