package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/state"
)

type BackupReader interface {
	ListBackupMessages(context.Context) ([]plugins.BackupMessage, error)
	ReadBackupMessage(context.Context, string) (*plugins.Message, error)
}

type MailboxAppender interface {
	EnsureMailbox(context.Context, string, bool) (bool, error)
	AppendMessage(context.Context, string, *plugins.Message) error
}

type ReceiptStore interface {
	HasRestoreReceipt(context.Context, state.RestoreReceipt) (bool, error)
	HasPendingRestore(context.Context, string, string, string) (bool, error)
	PendingRestoreLocators(context.Context, string, string, string) ([]string, error)
	BeginRestoreReceipt(context.Context, state.RestoreReceipt) error
	MarkRestoreReceipt(context.Context, state.RestoreReceipt) error
	BeginRestoreJob(context.Context, string, string, string) (bool, error)
	ActivateRestoreJob(context.Context, string, string, string) error
	AbandonRestoreJob(context.Context, string, string, string) error
}

type Options struct {
	BackupTarget      string
	DestinationSource string
	DestinationFolder string
	DryRun            bool
	AllowUnverified   bool
}

type Result struct {
	Messages   int
	Restored   int
	Skipped    int
	Planned    int
	Unverified int
}

func Run(ctx context.Context, reader BackupReader, appender MailboxAppender, receipts ReceiptStore, options Options) (Result, error) {
	if reader == nil {
		return Result{}, errors.New("backup reader is required")
	}
	if options.BackupTarget == "" || options.DestinationSource == "" || options.DestinationFolder == "" {
		return Result{}, errors.New("backup target, destination source, and destination folder are required")
	}
	if !options.DryRun && (appender == nil || receipts == nil) {
		return Result{}, errors.New("appender and restore receipt store are required")
	}
	entries, err := reader.ListBackupMessages(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("list backup messages: %w", err)
	}
	entries = append([]plugins.BackupMessage(nil), entries...)
	result := Result{Messages: len(entries)}
	if options.DryRun {
		for i, entry := range entries {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			_, normalized, unverified, err := readAndValidate(ctx, reader, entry, true)
			if err != nil {
				return result, err
			}
			entries[i] = normalized
			result.Planned++
			if unverified {
				result.Unverified++
			}
		}
		return result, nil
	}
	if len(entries) == 0 {
		return result, nil
	}
	pending, err := receipts.HasPendingRestore(ctx, options.DestinationSource, options.DestinationFolder, options.BackupTarget)
	if err != nil {
		return result, fmt.Errorf("check pending restore: %w", err)
	}
	if pending {
		locators, err := receipts.PendingRestoreLocators(ctx, options.DestinationSource, options.DestinationFolder, options.BackupTarget)
		if err != nil {
			return result, fmt.Errorf("list uncertain restore messages: %w", err)
		}
		return result, fmt.Errorf("prior APPEND outcome is uncertain for %s; inspect the destination, then pass the exact raw locator (without the quoted display marks or alias annotation) to `mailvault restore resolve --locator` with --action mark-restored or retry", strings.Join(locators, ", "))
	}
	for i, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		_, normalized, unverified, err := readAndValidate(ctx, reader, entry, options.AllowUnverified)
		if err != nil {
			return result, err
		}
		entries[i] = normalized
		if unverified {
			result.Unverified++
		}
	}
	resuming, err := receipts.BeginRestoreJob(ctx, options.DestinationSource, options.DestinationFolder, options.BackupTarget)
	if err != nil {
		return result, fmt.Errorf("begin restore job: %w", err)
	}
	created, err := appender.EnsureMailbox(ctx, options.DestinationFolder, resuming)
	if err != nil {
		if !resuming && (errors.Is(err, plugins.ErrMailboxExists) || errors.Is(err, plugins.ErrMailboxUnchanged)) {
			return result, errors.Join(fmt.Errorf("prepare destination mailbox: %w", err), receipts.AbandonRestoreJob(ctx, options.DestinationSource, options.DestinationFolder, options.BackupTarget))
		}
		return result, fmt.Errorf("prepare destination mailbox: %w", err)
	}
	if created {
		if err := receipts.ActivateRestoreJob(context.WithoutCancel(ctx), options.DestinationSource, options.DestinationFolder, options.BackupTarget); err != nil {
			return result, fmt.Errorf("activate restore job: %w", err)
		}
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		receipt := state.RestoreReceipt{
			DestinationSource: options.DestinationSource,
			DestinationFolder: options.DestinationFolder,
			BackupTarget:      options.BackupTarget,
			Locator:           entry.Locator,
			RawSHA256:         entry.RawSHA256,
			Source:            entry.Source,
			Scope:             entry.Scope,
			Folder:            entry.Folder,
			UID:               entry.UID,
		}
		exists, err := receipts.HasRestoreReceipt(ctx, receipt)
		if err != nil {
			return result, fmt.Errorf("check restore receipt: %w", err)
		}
		if exists {
			result.Skipped++
			continue
		}
		msg, _, _, err := readAndValidate(ctx, reader, entry, false)
		if err != nil {
			return result, err
		}
		if msg.UID == "" {
			msg.UID = entry.UID
		}
		if msg.UIDScope == "" {
			msg.UIDScope = entry.Scope
		}
		if msg.Folder == "" {
			msg.Folder = entry.Folder
		}
		if len(msg.Flags) == 0 {
			msg.Flags = append([]string(nil), entry.Flags...)
		}
		if msg.InternalDate == "" {
			msg.InternalDate = entry.InternalDate
		}
		if err := receipts.BeginRestoreReceipt(ctx, receipt); err != nil {
			return result, fmt.Errorf("record pending restore: %w", err)
		}
		if err := appender.AppendMessage(ctx, options.DestinationFolder, msg); err != nil {
			return result, err
		}
		if err := receipts.MarkRestoreReceipt(ctx, receipt); err != nil {
			return result, fmt.Errorf("record restored message: %w", err)
		}
		result.Restored++
	}
	return result, nil
}

func readAndValidate(ctx context.Context, reader BackupReader, entry plugins.BackupMessage, allowUnverified bool) (*plugins.Message, plugins.BackupMessage, bool, error) {
	msg, err := reader.ReadBackupMessage(ctx, entry.Locator)
	if err != nil {
		return nil, entry, false, fmt.Errorf("read backup message %q: %w", entry.Locator, err)
	}
	if msg == nil {
		return nil, entry, false, fmt.Errorf("backup message %q is empty", entry.Locator)
	}
	digest := sha256.Sum256(msg.Raw)
	unverified := entry.RawSHA256 == ""
	if unverified && !allowUnverified {
		return nil, entry, true, fmt.Errorf("legacy backup message %q has no manifest hash; review a dry run and pass --allow-unverified to restore it", entry.Locator)
	}
	if !unverified && !equalHexDigest(entry.RawSHA256, digest[:]) {
		return nil, entry, false, fmt.Errorf("backup message %q failed SHA-256 verification", entry.Locator)
	}
	if unverified {
		entry.RawSHA256 = hex.EncodeToString(digest[:])
	}
	return msg, entry, unverified, nil
}

func equalHexDigest(want string, digest []byte) bool {
	return len(want) == hex.EncodedLen(len(digest)) && hex.EncodeToString(digest) == want
}
