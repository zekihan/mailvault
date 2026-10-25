package restore

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	MailboxUIDValidity(context.Context, string) (string, error)
	AppendMessage(context.Context, string, *plugins.Message, string) error
}

type ReceiptStore interface {
	HasRestoreReceipt(context.Context, state.RestoreReceipt) (bool, error)
	HasPendingRestore(context.Context, string, string, string) (bool, error)
	PendingRestoreLocators(context.Context, string, string, string) ([]string, error)
	BeginRestoreReceipt(context.Context, state.RestoreReceipt) error
	MarkRestoreReceipt(context.Context, state.RestoreReceipt) error
	BeginRestoreJob(context.Context, string, string, string) (bool, string, error)
	ActivateRestoreJob(context.Context, string, string, string, string) error
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
	// Complete preflight before touching the destination. Spool normalized
	// metadata privately on disk instead of retaining every locator in memory.
	spool, err := os.CreateTemp("", "mailvault-restore-*.json")
	if err != nil {
		return Result{}, fmt.Errorf("create restore plan: %w", err)
	}
	defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
	writer := bufio.NewWriter(spool)
	encoder := json.NewEncoder(writer)
	result := Result{}
	seen := make(map[string]bool)
	err = plugins.WalkBackupMessages(ctx, reader, func(entry plugins.BackupMessage) error {
		result.Messages++
		_, normalized, unverified, err := readAndValidate(ctx, reader, entry, options.DryRun || options.AllowUnverified)
		if err != nil {
			return err
		}
		if options.DryRun {
			if !seen[normalized.RawSHA256] {
				result.Planned++
				if unverified {
					result.Unverified++
				}
				seen[normalized.RawSHA256] = true
			}
			return nil
		}
		if unverified {
			result.Unverified++
		}
		return encoder.Encode(normalized)
	})
	if err != nil {
		return result, fmt.Errorf("preflight backup messages: %w", err)
	}
	if err := writer.Flush(); err != nil {
		return result, fmt.Errorf("write restore plan: %w", err)
	}
	if options.DryRun || result.Messages == 0 {
		return result, nil
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	decoder := json.NewDecoder(bufio.NewReader(spool))
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
	resuming, expectedUIDValidity, err := receipts.BeginRestoreJob(ctx, options.DestinationSource, options.DestinationFolder, options.BackupTarget)
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
	uidValidity, err := appender.MailboxUIDValidity(ctx, options.DestinationFolder)
	if err != nil {
		return result, fmt.Errorf("read destination mailbox UIDVALIDITY: %w", err)
	}
	if uidValidity == "" || uidValidity == "0" {
		return result, errors.New("destination mailbox returned an empty or zero UIDVALIDITY")
	}
	if expectedUIDValidity != "" && uidValidity != expectedUIDValidity {
		return result, fmt.Errorf("destination mailbox UIDVALIDITY changed from %s to %s; choose a new destination folder", expectedUIDValidity, uidValidity)
	}
	if created || expectedUIDValidity == "" {
		if err := receipts.ActivateRestoreJob(context.WithoutCancel(ctx), options.DestinationSource, options.DestinationFolder, options.BackupTarget, uidValidity); err != nil {
			return result, fmt.Errorf("activate restore job: %w", err)
		}
	}
	for {
		var entry plugins.BackupMessage
		if err := decoder.Decode(&entry); err == io.EOF {
			break
		} else if err != nil {
			return result, fmt.Errorf("read restore plan: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		receipt := state.RestoreReceipt{
			DestinationSource:      options.DestinationSource,
			DestinationFolder:      options.DestinationFolder,
			BackupTarget:           options.BackupTarget,
			DestinationUIDValidity: uidValidity,
			Locator:                entry.Locator,
			RawSHA256:              entry.RawSHA256,
			Source:                 entry.Source,
			Scope:                  entry.Scope,
			Folder:                 entry.Folder,
			UID:                    entry.UID,
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
		currentUIDValidity, err := appender.MailboxUIDValidity(ctx, options.DestinationFolder)
		if err != nil {
			return result, fmt.Errorf("recheck destination mailbox UIDVALIDITY: %w", err)
		}
		if currentUIDValidity != uidValidity {
			return result, fmt.Errorf("destination mailbox UIDVALIDITY changed from %s to %s before APPEND", uidValidity, currentUIDValidity)
		}
		if err := receipts.BeginRestoreReceipt(ctx, receipt); err != nil {
			return result, fmt.Errorf("record pending restore: %w", err)
		}
		if err := appender.AppendMessage(ctx, options.DestinationFolder, msg, uidValidity); err != nil {
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
