package state

import (
	"context"
	"errors"
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
