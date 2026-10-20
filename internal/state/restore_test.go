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
		BackupTarget: "archive", Locator: "cur/message:2,", RawSHA256: "digest",
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
	if resuming, err := store.BeginRestoreJob(ctx, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget); err != nil || resuming {
		t.Fatalf("new restore job: resuming=%t err=%v", resuming, err)
	}
	if err := store.ActivateRestoreJob(ctx, receipt.DestinationSource, receipt.DestinationFolder, receipt.BackupTarget); err != nil {
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
	job := RestoreReceipt{DestinationSource: "account", DestinationFolder: "restore", BackupTarget: "archive", Locator: "object", RawSHA256: "digest"}
	if resuming, err := store.BeginRestoreJob(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget); err != nil || resuming {
		t.Fatalf("begin new job: resuming=%t err=%v", resuming, err)
	}
	if _, err := store.BeginRestoreJob(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget); !errors.Is(err, ErrRestoreJobUncertain) {
		t.Fatalf("preparing job was not blocked: %v", err)
	}
	if err := store.ActivateRestoreJob(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget); err != nil {
		t.Fatal(err)
	}
	if resuming, err := store.BeginRestoreJob(ctx, job.DestinationSource, job.DestinationFolder, job.BackupTarget); err != nil || !resuming {
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
