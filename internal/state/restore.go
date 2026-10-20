package state

import "context"

import (
	"database/sql"
	"errors"
	"fmt"
)

type RestoreReceipt struct {
	DestinationSource      string
	DestinationFolder      string
	BackupTarget           string
	DestinationUIDValidity string
	Locator                string
	RawSHA256              string
	Source                 string
	Scope                  string
	Folder                 string
	UID                    string
}

var ErrRestoreJobUncertain = errors.New("restore job setup has an uncertain outcome")
var ErrRestoreJobIdentityUnavailable = errors.New("restore job has no recorded destination UIDVALIDITY; choose a new destination folder")

func (s *Store) BeginRestoreJob(ctx context.Context, destinationSource, destinationFolder, backupTarget string) (bool, string, error) {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return false, "", err
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO restore_jobs
		(destination_source,destination_folder,backup_target,status) VALUES(?,?,?,'preparing')`, destinationSource, destinationFolder, backupTarget)
	if err != nil {
		return false, "", err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, "", err
	}
	if inserted == 1 {
		return false, "", nil
	}
	var status, uidValidity string
	var setupResolved bool
	err = s.db.QueryRowContext(ctx, `SELECT status,uid_validity,setup_resolved FROM restore_jobs WHERE destination_source=? AND destination_folder=? AND backup_target=?`, destinationSource, destinationFolder, backupTarget).Scan(&status, &uidValidity, &setupResolved)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", fmt.Errorf("restore job disappeared")
	}
	if err != nil {
		return false, "", err
	}
	if status == "active" {
		if uidValidity == "" {
			return false, "", ErrRestoreJobIdentityUnavailable
		}
		return true, uidValidity, nil
	}
	if setupResolved {
		return true, "", nil
	}
	return false, "", ErrRestoreJobUncertain
}

func (s *Store) ActivateRestoreJob(ctx context.Context, destinationSource, destinationFolder, backupTarget, uidValidity string) error {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return err
	}
	if uidValidity == "" || uidValidity == "0" {
		return errors.New("destination mailbox returned an empty or zero UIDVALIDITY")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE restore_jobs SET status='active',uid_validity=?,setup_resolved=0
		WHERE destination_source=? AND destination_folder=? AND backup_target=? AND status='preparing'`, uidValidity, destinationSource, destinationFolder, backupTarget)
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
		result, err := s.db.ExecContext(ctx, `UPDATE restore_jobs SET setup_resolved=1
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
	if receipt.DestinationUIDValidity == "" || receipt.DestinationUIDValidity == "0" {
		return false, errors.New("destination UIDVALIDITY is required to check a restore receipt")
	}
	if err := s.migrateLegacyRestoreReceipts(ctx, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget, receipt.DestinationUIDValidity); err != nil {
		return false, err
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM restore_receipt_identities
		WHERE destination_source=? AND destination_folder=? AND backup_target=? AND uid_validity=? AND raw_sha256=?
	)`, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget, receipt.DestinationUIDValidity, receipt.RawSHA256).Scan(&exists)
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
	if err := insertRestoreIdentity(ctx, tx, receipt); err != nil {
		return err
	}
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

func insertRestoreIdentity(ctx context.Context, tx *sql.Tx, receipt RestoreReceipt) error {
	if receipt.DestinationUIDValidity == "" || receipt.DestinationUIDValidity == "0" {
		return errors.New("destination UIDVALIDITY is required to record a restore receipt")
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO restore_receipt_identities
		(destination_source,destination_folder,backup_target,uid_validity,raw_sha256)
		VALUES(?,?,?,?,?)`, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget, receipt.DestinationUIDValidity, receipt.RawSHA256)
	return err
}

func (s *Store) migrateLegacyRestoreReceipts(ctx context.Context, destinationSource, destinationFolder, backupTarget, uidValidity string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO restore_receipt_migrations
		(destination_source,destination_folder,backup_target,uid_validity)
		SELECT ?,?,?,? WHERE EXISTS(SELECT 1 FROM restore_jobs
		WHERE destination_source=? AND destination_folder=? AND backup_target=? AND status='active' AND uid_validity=?)`,
		destinationSource, destinationFolder, backupTarget, uidValidity,
		destinationSource, destinationFolder, backupTarget, uidValidity)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted != 1 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO restore_receipt_identities
		(destination_source,destination_folder,backup_target,uid_validity,raw_sha256)
		SELECT r.destination_source,r.destination_folder,r.backup_target,j.uid_validity,r.raw_sha256
		FROM restore_receipts r JOIN restore_jobs j
		ON j.destination_source=r.destination_source AND j.destination_folder=r.destination_folder AND j.backup_target=r.backup_target
		WHERE r.destination_source=? AND r.destination_folder=? AND r.backup_target=?
		AND j.status='active' AND j.uid_validity=? AND length(r.raw_sha256)=64 AND r.raw_sha256 NOT GLOB '*[^0-9a-f]*'`,
		destinationSource, destinationFolder, backupTarget, uidValidity); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BeginRestoreReceipt(ctx context.Context, receipt RestoreReceipt) error {
	if err := s.ensureRestoreReceipts(ctx); err != nil {
		return err
	}
	if receipt.DestinationUIDValidity == "" || receipt.DestinationUIDValidity == "0" {
		return errors.New("destination UIDVALIDITY is required to record a pending restore")
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
		var uidValidity string
		if err := tx.QueryRowContext(ctx, `SELECT uid_validity FROM restore_jobs
			WHERE destination_source=? AND destination_folder=? AND backup_target=? AND status='active'`,
			destinationSource, destinationFolder, backupTarget).Scan(&uidValidity); err != nil {
			return fmt.Errorf("restore job has no active destination generation: %w", err)
		}
		if err := insertRestoreIdentity(ctx, tx, RestoreReceipt{
			DestinationSource: destinationSource, DestinationFolder: destinationFolder,
			BackupTarget: backupTarget, DestinationUIDValidity: uidValidity, RawSHA256: item.digest,
		}); err != nil {
			return err
		}
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
	CREATE TABLE IF NOT EXISTS restore_receipt_identities (
		destination_source TEXT NOT NULL,
		destination_folder TEXT NOT NULL,
		backup_target TEXT NOT NULL,
		uid_validity TEXT NOT NULL,
		raw_sha256 TEXT NOT NULL,
		restored_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(destination_source,destination_folder,backup_target,uid_validity,raw_sha256)
	);
	CREATE TABLE IF NOT EXISTS restore_receipt_migrations (
		destination_source TEXT NOT NULL,
		destination_folder TEXT NOT NULL,
		backup_target TEXT NOT NULL,
		uid_validity TEXT NOT NULL,
		migrated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(destination_source,destination_folder,backup_target,uid_validity)
	);
	CREATE TABLE IF NOT EXISTS restore_jobs (
		destination_source TEXT NOT NULL,
		destination_folder TEXT NOT NULL,
		backup_target TEXT NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('preparing','active')),
		uid_validity TEXT NOT NULL DEFAULT '',
		setup_resolved INTEGER NOT NULL DEFAULT 0,
		started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(destination_source,destination_folder,backup_target)
	)`)
	if err != nil {
		return err
	}
	columns, err := s.db.QueryContext(ctx, `PRAGMA table_info(restore_jobs)`)
	if err != nil {
		return err
	}
	hasUIDValidity, hasSetupResolved := false, false
	for columns.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := columns.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = columns.Close()
			return err
		}
		hasUIDValidity = hasUIDValidity || name == "uid_validity"
		hasSetupResolved = hasSetupResolved || name == "setup_resolved"
	}
	if err := errors.Join(columns.Err(), columns.Close()); err != nil {
		return err
	}
	if !hasUIDValidity {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE restore_jobs ADD COLUMN uid_validity TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasSetupResolved {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE restore_jobs ADD COLUMN setup_resolved INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return nil
}
