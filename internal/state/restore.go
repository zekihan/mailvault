package state

import "context"

import (
	"database/sql"
	"errors"
	"fmt"
)

type RestoreReceipt struct {
	DestinationSource string
	DestinationFolder string
	BackupTarget      string
	Locator           string
	RawSHA256         string
	Source            string
	Scope             string
	Folder            string
	UID               string
}

var ErrRestoreJobUncertain = errors.New("restore job setup has an uncertain outcome")

func (s *Store) BeginRestoreJob(ctx context.Context, destinationSource, destinationFolder, backupTarget string) (bool, error) {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO restore_jobs
		(destination_source,destination_folder,backup_target,status) VALUES(?,?,?,'preparing')`, destinationSource, destinationFolder, backupTarget)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if inserted == 1 {
		return false, nil
	}
	var status string
	err = s.db.QueryRowContext(ctx, `SELECT status FROM restore_jobs WHERE destination_source=? AND destination_folder=? AND backup_target=?`, destinationSource, destinationFolder, backupTarget).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("restore job disappeared")
	}
	if err != nil {
		return false, err
	}
	if status != "active" {
		return false, ErrRestoreJobUncertain
	}
	return true, nil
}

func (s *Store) ActivateRestoreJob(ctx context.Context, destinationSource, destinationFolder, backupTarget string) error {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE restore_jobs SET status='active'
		WHERE destination_source=? AND destination_folder=? AND backup_target=? AND status='preparing'`, destinationSource, destinationFolder, backupTarget)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count != 1 {
		return errors.New("restore job is not preparing")
	}
	return nil
}

func (s *Store) AbandonRestoreJob(ctx context.Context, destinationSource, destinationFolder, backupTarget string) error {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM restore_jobs
		WHERE destination_source=? AND destination_folder=? AND backup_target=? AND status='preparing'
		AND NOT EXISTS (SELECT 1 FROM restore_receipts WHERE destination_source=? AND destination_folder=? AND backup_target=?)`,
		destinationSource, destinationFolder, backupTarget, destinationSource, destinationFolder, backupTarget)
	return err
}

func (s *Store) ResolveRestoreJobSetup(ctx context.Context, destinationSource, destinationFolder, backupTarget string, resume bool) error {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return err
	}
	if resume {
		result, err := s.db.ExecContext(ctx, `UPDATE restore_jobs SET status='active'
			WHERE destination_source=? AND destination_folder=? AND backup_target=? AND status='preparing'`, destinationSource, destinationFolder, backupTarget)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil {
			return err
		} else if count != 1 {
			return errors.New("restore job is not awaiting setup resolution")
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM restore_jobs
		WHERE destination_source=? AND destination_folder=? AND backup_target=? AND status='preparing'
		AND NOT EXISTS (SELECT 1 FROM restore_receipts WHERE destination_source=? AND destination_folder=? AND backup_target=?)`,
		destinationSource, destinationFolder, backupTarget, destinationSource, destinationFolder, backupTarget)
	return err
}

func (s *Store) HasRestoreReceipt(ctx context.Context, receipt RestoreReceipt) (bool, error) {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return false, err
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM restore_receipts
		WHERE destination_source=? AND destination_folder=? AND backup_target=? AND locator=? AND raw_sha256=?
		AND source_name=? AND scope=? AND original_folder=? AND uid=?
	)`, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget, receipt.Locator, receipt.RawSHA256, receipt.Source, receipt.Scope, receipt.Folder, receipt.UID).Scan(&exists)
	return exists, err
}

func (s *Store) MarkRestoreReceipt(ctx context.Context, receipt RestoreReceipt) error {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO restore_receipts
		(destination_source,destination_folder,backup_target,locator,raw_sha256,source_name,scope,original_folder,uid)
		VALUES(?,?,?,?,?,?,?,?,?)`, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget, receipt.Locator, receipt.RawSHA256, receipt.Source, receipt.Scope, receipt.Folder, receipt.UID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM restore_pending
		WHERE destination_source=? AND destination_folder=? AND backup_target=? AND locator=? AND raw_sha256=?
		AND source_name=? AND scope=? AND original_folder=? AND uid=?`,
		receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget, receipt.Locator, receipt.RawSHA256, receipt.Source, receipt.Scope, receipt.Folder, receipt.UID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BeginRestoreReceipt(ctx context.Context, receipt RestoreReceipt) error {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO restore_pending
		(destination_source,destination_folder,backup_target,locator,raw_sha256,source_name,scope,original_folder,uid)
		VALUES(?,?,?,?,?,?,?,?,?)`, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget, receipt.Locator, receipt.RawSHA256, receipt.Source, receipt.Scope, receipt.Folder, receipt.UID)
	return err
}

func (s *Store) HasRestoreJob(ctx context.Context, destinationSource, destinationFolder, backupTarget string) (bool, error) {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return false, err
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM restore_jobs WHERE destination_source=? AND destination_folder=? AND backup_target=? AND status='active'
	)`, destinationSource, destinationFolder, backupTarget).Scan(&exists)
	return exists, err
}

func (s *Store) ResolvePendingRestore(ctx context.Context, destinationSource, destinationFolder, backupTarget, locator string, markRestored bool) error {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT raw_sha256,source_name,scope,original_folder,uid FROM restore_pending
		WHERE destination_source=? AND destination_folder=? AND backup_target=? AND locator=?`,
		destinationSource, destinationFolder, backupTarget, locator)
	if err != nil {
		return err
	}
	type pendingIdentity struct {
		digest, sourceName, scope, originalFolder, uid string
	}
	var pending []pendingIdentity
	for rows.Next() {
		var item pendingIdentity
		if err := rows.Scan(&item.digest, &item.sourceName, &item.scope, &item.originalFolder, &item.uid); err != nil {
			_ = rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(pending) != 1 {
		return fmt.Errorf("expected one pending message for locator %q; found %d", locator, len(pending))
	}
	item := pending[0]
	if markRestored {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO restore_receipts
			(destination_source,destination_folder,backup_target,locator,raw_sha256,source_name,scope,original_folder,uid) VALUES(?,?,?,?,?,?,?,?,?)`,
			destinationSource, destinationFolder, backupTarget, locator, item.digest, item.sourceName, item.scope, item.originalFolder, item.uid); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM restore_pending WHERE destination_source=? AND destination_folder=? AND backup_target=? AND locator=? AND raw_sha256=? AND source_name=? AND scope=? AND original_folder=? AND uid=?`,
		destinationSource, destinationFolder, backupTarget, locator, item.digest, item.sourceName, item.scope, item.originalFolder, item.uid); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) HasPendingRestore(ctx context.Context, destinationSource, destinationFolder, backupTarget string) (bool, error) {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return false, err
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM restore_pending WHERE destination_source=? AND destination_folder=? AND backup_target=?
	)`, destinationSource, destinationFolder, backupTarget).Scan(&exists)
	return exists, err
}

func (s *Store) PendingRestoreLocators(ctx context.Context, destinationSource, destinationFolder, backupTarget string) (locators []string, resultErr error) {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT locator,source_name,scope,original_folder,uid FROM restore_pending
		WHERE destination_source=? AND destination_folder=? AND backup_target=? ORDER BY locator`,
		destinationSource, destinationFolder, backupTarget)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	for rows.Next() {
		var locator, sourceName, scope, originalFolder, uid string
		if err := rows.Scan(&locator, &sourceName, &scope, &originalFolder, &uid); err != nil {
			return nil, err
		}
		locators = append(locators, fmt.Sprintf("%q [source=%q scope=%q folder=%q uid=%q]", locator, sourceName, scope, originalFolder, uid))
	}
	return locators, rows.Err()
}

func (s *Store) ensureRestoreReceipts(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS restore_receipts (
		destination_source TEXT NOT NULL,
		destination_folder TEXT NOT NULL,
		backup_target TEXT NOT NULL,
		locator TEXT NOT NULL,
		raw_sha256 TEXT NOT NULL,
		source_name TEXT NOT NULL,
		scope TEXT NOT NULL,
		original_folder TEXT NOT NULL,
		uid TEXT NOT NULL,
		restored_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(destination_source,destination_folder,backup_target,locator,raw_sha256,source_name,scope,original_folder,uid)
	);
	CREATE TABLE IF NOT EXISTS restore_pending (
		destination_source TEXT NOT NULL,
		destination_folder TEXT NOT NULL,
		backup_target TEXT NOT NULL,
		locator TEXT NOT NULL,
		raw_sha256 TEXT NOT NULL,
		source_name TEXT NOT NULL,
		scope TEXT NOT NULL,
		original_folder TEXT NOT NULL,
		uid TEXT NOT NULL,
		started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(destination_source,destination_folder,backup_target,locator,raw_sha256,source_name,scope,original_folder,uid)
	);
	CREATE TABLE IF NOT EXISTS restore_jobs (
		destination_source TEXT NOT NULL,
		destination_folder TEXT NOT NULL,
		backup_target TEXT NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('preparing','active')),
		started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(destination_source,destination_folder,backup_target)
	)`)
	return err
}
