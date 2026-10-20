package state

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestRestoreReceiptsAreScopedAndIdempotent(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	receipt := RestoreReceipt{
		DestinationSource: "account", DestinationFolder: "Mailvault Restore",
		BackupTarget: "archive", DestinationUIDValidity: "7", Locator: "cur/message:2,", RawSHA256: "digest",
	}
	if exists, err := store.HasRestoreReceipt(ctx, receipt); err != nil || exists {
		t.Fatalf("initial receipt: exists=%t err=%v", exists, err)
	}
	if err := store.MarkRestoreReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRestoreReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.HasRestoreReceipt(ctx, receipt); err != nil || !exists {
		t.Fatalf("recorded receipt: exists=%t err=%v", exists, err)
	}
	if resuming, _, err := store.BeginRestoreJob(ctx, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget); err != nil || resuming {
		t.Fatalf("new restore job: resuming=%t err=%v", resuming, err)
	}
	if err := store.ActivateRestoreJob(ctx, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget, "7"); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.HasRestoreJob(ctx, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget); err != nil || !exists {
		t.Fatalf("restore job: exists=%t err=%v", exists, err)
	}
	if err := store.BeginRestoreReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.HasPendingRestore(ctx, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget); err != nil || !pending {
		t.Fatalf("pending receipt: exists=%t err=%v", pending, err)
	}
	if err := store.MarkRestoreReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.HasPendingRestore(ctx, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget); err != nil || pending {
		t.Fatalf("completed receipt retained pending marker: exists=%t err=%v", pending, err)
	}
	otherFolder := receipt
	otherFolder.DestinationFolder = "Other"
	if exists, err := store.HasRestoreReceipt(ctx, otherFolder); err != nil || exists {
		t.Fatalf("receipt leaked across destination folders: exists=%t err=%v", exists, err)
	}
}

func TestRestoreJobSetupAndUncertainResolution(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	job := RestoreReceipt{DestinationSource: "account", DestinationFolder: "restore", BackupTarget: "archive", DestinationUIDValidity: "7", Locator: "object", RawSHA256: "digest"}
	if resuming, _, err := store.BeginRestoreJob(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget); err != nil || resuming {
		t.Fatalf("begin new job: resuming=%t err=%v", resuming, err)
	}
	if _, _, err := store.BeginRestoreJob(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget); !errors.Is(err, ErrRestoreJobUncertain) {
		t.Fatalf("preparing job was not blocked: %v", err)
	}
	if err := store.ActivateRestoreJob(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget, "7"); err != nil {
		t.Fatal(err)
	}
	if resuming, uidValidity, err := store.BeginRestoreJob(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget); err != nil || !resuming || uidValidity != "7" {
		t.Fatalf("resume active job: resuming=%t err=%v", resuming, err)
	}
	if err := store.BeginRestoreReceipt(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolvePendingRestore(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget, job.Locator, true); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.HasRestoreReceipt(ctx, job); err != nil || !exists {
		t.Fatalf("mark-restored did not record receipt: exists=%t err=%v", exists, err)
	}
	if pending, err := store.HasPendingRestore(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget); err != nil || pending {
		t.Fatalf("resolved state remains pending: pending=%t err=%v", pending, err)
	}
}

func TestRestoreReceiptUsesContentIdentityWithinMailboxGeneration(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	receipt := RestoreReceipt{DestinationSource: "account", DestinationFolder: "restore", BackupTarget: "archive#fingerprint", DestinationUIDValidity: "17", Locator: "maildir/old", RawSHA256: "same-content"}
	if _, _, err := store.BeginRestoreJob(ctx, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateRestoreJob(ctx, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget, receipt.DestinationUIDValidity); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRestoreReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	repaired := receipt
	repaired.Locator = "maildir/repaired"
	repaired.Source = "renamed account"
	repaired.Scope = "new scope"
	repaired.Folder = "renamed folder"
	repaired.UID = "99"
	if exists, err := store.HasRestoreReceipt(ctx, repaired); err != nil || !exists {
		t.Fatalf("same content at repaired locator was not recognized: exists=%t err=%v", exists, err)
	}
	newGeneration := repaired
	newGeneration.DestinationUIDValidity = "18"
	if exists, err := store.HasRestoreReceipt(ctx, newGeneration); err != nil || exists {
		t.Fatalf("receipt leaked across destination generations: exists=%t err=%v", exists, err)
	}
}

func TestLegacyRestoreJobWithoutGenerationFailsClosed(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	job := RestoreReceipt{DestinationSource: "account", DestinationFolder: "restore", BackupTarget: "archive"}
	if _, _, err := store.BeginRestoreJob(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateRestoreJob(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget, "7"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE restore_jobs SET uid_validity='' WHERE destination_source=? AND destination_folder=? AND backup_target=?`, job.DestinationSource, job.DestinationFolder, job.BackupTarget); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BeginRestoreJob(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget); !errors.Is(err, ErrRestoreJobIdentityUnavailable) {
		t.Fatalf("legacy job without generation was not rejected: %v", err)
	}
}

func TestLegacyRestoreReceiptMigrationRunsOncePerGeneration(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	job := RestoreReceipt{DestinationSource: "account", DestinationFolder: "restore", BackupTarget: "archive", DestinationUIDValidity: "7"}
	if err := insertLegacyRestoreReceipts(ctx, store, job, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TABLE migration_attempts (count INTEGER NOT NULL);
		INSERT INTO migration_attempts VALUES(0);
		CREATE TRIGGER count_restore_identity_inserts BEFORE INSERT ON restore_receipt_identities
		BEGIN UPDATE migration_attempts SET count=count+1; END`); err != nil {
		t.Fatal(err)
	}
	// A lookup before activation must not persist a completed migration marker.
	probe := job
	probe.RawSHA256 = fmt.Sprintf("%064x", 1)
	if exists, err := store.HasRestoreReceipt(ctx, probe); err != nil || exists {
		t.Fatalf("pre-activation lookup: exists=%t err=%v", exists, err)
	}
	if err := store.ActivateRestoreJob(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget, job.DestinationUIDValidity); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.HasRestoreReceipt(ctx, probe); err != nil || !exists {
		t.Fatalf("legacy receipt was not migrated after activation: exists=%t err=%v", exists, err)
	}
	for i := 0; i < 20; i++ {
		if _, err := store.HasRestoreReceipt(ctx, probe); err != nil {
			t.Fatal(err)
		}
	}
	var attempts int
	if err := store.db.QueryRowContext(ctx, `SELECT count FROM migration_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 100 {
		t.Fatalf("legacy identities attempted %d inserts; want 100 once", attempts)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 20; i++ {
		if exists, err := store.HasRestoreReceipt(ctx, probe); err != nil || !exists {
			t.Fatalf("lookup after reopen: exists=%t err=%v", exists, err)
		}
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count FROM migration_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 100 {
		t.Fatalf("reopen retried legacy inserts: got %d attempts, want 100", attempts)
	}
	newReceipt := job
	newReceipt.RawSHA256 = "new-content"
	if err := store.MarkRestoreReceipt(ctx, newReceipt); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.HasRestoreReceipt(ctx, newReceipt); err != nil || !exists {
		t.Fatalf("receipt written after migration was not visible: exists=%t err=%v", exists, err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count FROM migration_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 101 {
		t.Fatalf("normal receipt insert count=%d; want migration 100 plus one new receipt", attempts)
	}
}

func TestLegacyRestoreReceiptMigrationFailureCanRetry(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	job := RestoreReceipt{DestinationSource: "account", DestinationFolder: "restore", BackupTarget: "archive", DestinationUIDValidity: "7", RawSHA256: fmt.Sprintf("%064x", 1)}
	if err := insertLegacyRestoreReceipts(ctx, store, job, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateRestoreJob(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget, job.DestinationUIDValidity); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_restore_identity_insert BEFORE INSERT ON restore_receipt_identities
		BEGIN SELECT RAISE(ABORT, 'injected migration failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HasRestoreReceipt(ctx, job); err == nil {
		t.Fatal("lookup succeeded despite injected migration failure")
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM restore_receipt_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed migration left %d marker(s)", count)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM restore_receipt_identities`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed migration left %d identity row(s)", count)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_restore_identity_insert`); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.HasRestoreReceipt(ctx, job); err != nil || !exists {
		t.Fatalf("migration retry failed: exists=%t err=%v", exists, err)
	}
}

func insertLegacyRestoreReceipts(ctx context.Context, store *Store, job RestoreReceipt, count int) error {
	if err := store.ensureRestoreReceipts(ctx); err != nil {
		return err
	}
	_, err := store.db.ExecContext(ctx, `INSERT INTO restore_jobs
		(destination_source,destination_folder,backup_target,status,uid_validity) VALUES(?,?,?,'preparing','')`,
		job.DestinationSource, job.DestinationFolder, job.BackupTarget)
	if err != nil {
		return err
	}
	for i := 0; i < count; i++ {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO restore_receipts
			(destination_source,destination_folder,backup_target,locator,raw_sha256,source_name,scope,original_folder,uid)
			VALUES(?,?,?,?,?,'source','scope','folder',?)`,
			job.DestinationSource, job.DestinationFolder, job.BackupTarget, fmt.Sprintf("legacy-%d", i), fmt.Sprintf("%064x", i+1), fmt.Sprint(i+1)); err != nil {
			return err
		}
	}
	return nil
}
