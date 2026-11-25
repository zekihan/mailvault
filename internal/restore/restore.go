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
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/state"
)

type BackupReader interface {
	ListBackupMessages(ctx context.Context) ([]plugins.BackupMessage, error)
	ReadBackupMessage(ctx context.Context, locator string) (*plugins.Message, error)
}

type MailboxAppender interface {
	EnsureMailbox(ctx context.Context, mailbox string, allowExisting bool) (bool, error)
	MailboxUIDValidity(ctx context.Context, mailbox string) (string, error)
	AppendMessage(ctx context.Context, mailbox string, msg *plugins.Message, raw string) error
}

type ReceiptStore interface {
	HasRestoreReceipt(ctx context.Context, receipt *state.RestoreReceipt) (bool, error)
	HasPendingRestore(ctx context.Context, destinationSource, destinationFolder, backupTarget string) (bool, error)
	PendingRestoreLocators(ctx context.Context, destinationSource, destinationFolder, backupTarget string) ([]string, error)
	BeginRestoreReceipt(ctx context.Context, receipt *state.RestoreReceipt) error
	MarkRestoreReceipt(ctx context.Context, receipt *state.RestoreReceipt) error
	BeginRestoreJob(ctx context.Context, destinationSource, destinationFolder, backupTarget string) (bool, string, error)
	ActivateRestoreJob(ctx context.Context, destinationSource, destinationFolder, backupTarget, uidValidity string) error
	AbandonRestoreJob(ctx context.Context, destinationSource, destinationFolder, backupTarget string) error
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
	if err := checkRunOptions(reader, appender, receipts, options); err != nil {
		return Result{}, err
	}
	preflight := newRestorePreflight(reader, options)
	if err := preflight.openSpool(); err != nil {
		return Result{}, err
	}
	if preflight.spool != nil {
		defer func() { _ = preflight.spool.Close(); _ = os.Remove(preflight.spool.Name()) }()
	}
	spool, writer := preflight.spool, preflight.writer
	result := Result{}
	preflight.result = &result
	err := plugins.WalkBackupMessages(ctx, reader, func(entry plugins.BackupMessage) error {
		return preflight.walkEntry(ctx, &entry)
	})
	if err != nil {
		return result, fmt.Errorf("preflight backup messages: %w", err)
	}
	if options.DryRun || result.Messages == 0 {
		return result, nil
	}
	return replayRestorePlan(ctx, reader, appender, receipts, options, &result, spool, writer)
}

func checkRunOptions(reader BackupReader, appender MailboxAppender, receipts ReceiptStore, options Options) error {
	if reader == nil {
		return errors.New("backup reader is required")
	}
	if options.BackupTarget == "" || options.DestinationSource == "" || options.DestinationFolder == "" {
		return errors.New("backup target, destination source, and destination folder are required")
	}
	if !options.DryRun && (appender == nil || receipts == nil) {
		return errors.New("appender and restore receipt store are required")
	}
	return nil
}

type restorePreflight struct {
	reader  BackupReader
	options Options
	result  *Result
	seen    map[string]bool
	spool   *os.File
	writer  *bufio.Writer
	encoder *json.Encoder
}

func newRestorePreflight(reader BackupReader, options Options) *restorePreflight {
	return &restorePreflight{reader: reader, options: options, seen: make(map[string]bool)}
}

func (preflight *restorePreflight) openSpool() error {
	if preflight.options.DryRun {
		return nil
	}
	// Spool normalized metadata privately until preflight has verified every message.
	spool, err := os.CreateTemp("", "mailvault-restore-*.json")
	if err != nil {
		return fmt.Errorf("create restore plan: %w", err)
	}
	preflight.spool = spool
	preflight.writer = bufio.NewWriter(spool)
	preflight.encoder = json.NewEncoder(preflight.writer)
	return nil
}

func (preflight *restorePreflight) walkEntry(ctx context.Context, entry *plugins.BackupMessage) error {
	preflight.result.Messages++
	_, normalized, unverified, err := readAndValidate(ctx, preflight.reader, entry, preflight.options.DryRun || preflight.options.AllowUnverified)
	if err != nil {
		return err
	}
	if preflight.options.DryRun {
		planPreflightEntry(preflight.result, preflight.seen, &normalized, unverified)
		return nil
	}
	if unverified {
		preflight.result.Unverified++
	}
	return preflight.encoder.Encode(normalized)
}

func planPreflightEntry(result *Result, seen map[string]bool, normalized *plugins.BackupMessage, unverified bool) {
	if seen[normalized.RawSHA256] {
		return
	}
	result.Planned++
	if unverified {
		result.Unverified++
	}
	seen[normalized.RawSHA256] = true
}

func replayRestorePlan(ctx context.Context, reader BackupReader, appender MailboxAppender, receipts ReceiptStore, options Options, result *Result, spool *os.File, writer *bufio.Writer) (Result, error) {
	if err := writer.Flush(); err != nil {
		return *result, fmt.Errorf("write restore plan: %w", err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return *result, err
	}
	if err := checkPendingRestore(ctx, receipts, options); err != nil {
		return *result, err
	}
	uidValidity, err := prepareRestoreMailbox(ctx, appender, receipts, options)
	if err != nil {
		return *result, err
	}
	decoder := json.NewDecoder(bufio.NewReader(spool))
	return replayRestoreEntries(ctx, reader, appender, receipts, options, result, decoder, uidValidity)
}

func checkPendingRestore(ctx context.Context, receipts ReceiptStore, options Options) error {
	pending, err := receipts.HasPendingRestore(ctx, options.DestinationSource, options.DestinationFolder, options.BackupTarget)
	if err != nil {
		return fmt.Errorf("check pending restore: %w", err)
	}
	if !pending {
		return nil
	}
	locators, err := receipts.PendingRestoreLocators(ctx, options.DestinationSource, options.DestinationFolder, options.BackupTarget)
	if err != nil {
		return fmt.Errorf("list uncertain restore messages: %w", err)
	}
	return fmt.Errorf("prior APPEND outcome is uncertain for %s; inspect the destination, then pass the exact raw locator (without the quoted display marks or alias annotation) to `mailvault restore resolve --locator` with --action mark-restored or retry", strings.Join(locators, ", "))
}

func prepareRestoreMailbox(ctx context.Context, appender MailboxAppender, receipts ReceiptStore, options Options) (string, error) {
	resuming, expectedUIDValidity, err := receipts.BeginRestoreJob(ctx, options.DestinationSource, options.DestinationFolder, options.BackupTarget)
	if err != nil {
		return "", fmt.Errorf("begin restore job: %w", err)
	}
	created, err := appender.EnsureMailbox(ctx, options.DestinationFolder, resuming)
	if err != nil {
		return "", mailboxPrepareError(ctx, err, receipts, options, resuming)
	}
	uidValidity, err := appender.MailboxUIDValidity(ctx, options.DestinationFolder)
	if err != nil {
		return "", fmt.Errorf("read destination mailbox UIDVALIDITY: %w", err)
	}
	if err := checkUIDValidity(uidValidity, expectedUIDValidity); err != nil {
		return "", err
	}
	if created || expectedUIDValidity == "" {
		if err := receipts.ActivateRestoreJob(context.WithoutCancel(ctx), options.DestinationSource, options.DestinationFolder, options.BackupTarget, uidValidity); err != nil {
			return "", fmt.Errorf("activate restore job: %w", err)
		}
	}
	return uidValidity, nil
}

func mailboxPrepareError(ctx context.Context, err error, receipts ReceiptStore, options Options, resuming bool) error {
	if !resuming && (errors.Is(err, plugins.ErrMailboxExists) || errors.Is(err, plugins.ErrMailboxUnchanged)) {
		return errors.Join(fmt.Errorf("prepare destination mailbox: %w", err), receipts.AbandonRestoreJob(ctx, options.DestinationSource, options.DestinationFolder, options.BackupTarget))
	}
	return fmt.Errorf("prepare destination mailbox: %w", err)
}

func checkUIDValidity(uidValidity, expected string) error {
	if uidValidity == "" || uidValidity == "0" {
		return errors.New("destination mailbox returned an empty or zero UIDVALIDITY")
	}
	if expected != "" && uidValidity != expected {
		return fmt.Errorf("destination mailbox UIDVALIDITY changed from %s to %s; choose a new destination folder", expected, uidValidity)
	}
	return nil
}

func replayRestoreEntries(ctx context.Context, reader BackupReader, appender MailboxAppender, receipts ReceiptStore, options Options, result *Result, decoder *json.Decoder, uidValidity string) (Result, error) {
	for {
		var entry plugins.BackupMessage
		if err := decoder.Decode(&entry); err == io.EOF {
			break
		} else if err != nil {
			return *result, fmt.Errorf("read restore plan: %w", err)
		}
		if err := restorePlanEntry(ctx, reader, appender, receipts, options, result, &entry, uidValidity); err != nil {
			return *result, err
		}
	}
	return *result, nil
}

func restorePlanEntry(ctx context.Context, reader BackupReader, appender MailboxAppender, receipts ReceiptStore, options Options, result *Result, entry *plugins.BackupMessage, uidValidity string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	receipt := newRestoreReceipt(options, entry, uidValidity)
	exists, err := receipts.HasRestoreReceipt(ctx, receipt)
	if err != nil {
		return fmt.Errorf("check restore receipt: %w", err)
	}
	if exists {
		result.Skipped++
		return nil
	}
	if err := appendRestoreMessage(ctx, reader, appender, receipts, options, entry, uidValidity, receipt); err != nil {
		return err
	}
	result.Restored++
	return nil
}

func newRestoreReceipt(options Options, entry *plugins.BackupMessage, uidValidity string) *state.RestoreReceipt {
	return &state.RestoreReceipt{
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
}

func appendRestoreMessage(ctx context.Context, reader BackupReader, appender MailboxAppender, receipts ReceiptStore, options Options, entry *plugins.BackupMessage, uidValidity string, receipt *state.RestoreReceipt) error {
	msg, _, _, err := readAndValidate(ctx, reader, entry, false)
	if err != nil {
		return err
	}
	currentUIDValidity, err := appender.MailboxUIDValidity(ctx, options.DestinationFolder)
	if err != nil {
		return fmt.Errorf("recheck destination mailbox UIDVALIDITY: %w", err)
	}
	if currentUIDValidity != uidValidity {
		return fmt.Errorf("destination mailbox UIDVALIDITY changed from %s to %s before APPEND", uidValidity, currentUIDValidity)
	}
	if err := receipts.BeginRestoreReceipt(ctx, receipt); err != nil {
		return fmt.Errorf("record pending restore: %w", err)
	}
	if err := appender.AppendMessage(ctx, options.DestinationFolder, msg, uidValidity); err != nil {
		return err
	}
	if err := receipts.MarkRestoreReceipt(ctx, receipt); err != nil {
		return fmt.Errorf("record restored message: %w", err)
	}
	return nil
}

func readAndValidate(ctx context.Context, reader BackupReader, entry *plugins.BackupMessage, allowUnverified bool) (*plugins.Message, plugins.BackupMessage, bool, error) {
	normalized := *entry
	msg, err := reader.ReadBackupMessage(ctx, entry.Locator)
	if err != nil {
		return nil, normalized, false, fmt.Errorf("read backup message %q: %w", entry.Locator, err)
	}
	if msg == nil {
		return nil, normalized, false, fmt.Errorf("backup message %q is empty", entry.Locator)
	}
	// Work on a copy: readers may reuse their message object across reads.
	cloned := *msg
	msg = &cloned
	if err := normalizeRestoreMessage(msg, &normalized); err != nil {
		return nil, normalized, false, err
	}
	digest := sha256.Sum256(msg.Raw)
	unverified := normalized.RawSHA256 == ""
	if err := verifyRestoreHash(&normalized, digest, unverified, allowUnverified); err != nil {
		return nil, normalized, unverified, err
	}
	if unverified {
		normalized.RawSHA256 = hex.EncodeToString(digest[:])
	}
	return msg, normalized, unverified, nil
}

func normalizeRestoreMessage(msg *plugins.Message, entry *plugins.BackupMessage) error {
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
	if msg.InternalDate != "" {
		if _, err := time.Parse(time.RFC3339, msg.InternalDate); err != nil {
			return fmt.Errorf("backup message %q has invalid internal date: %w", entry.Locator, err)
		}
	}
	if _, err := plugins.NormalizeAppendFlags(msg.Flags); err != nil {
		return fmt.Errorf("backup message %q has invalid flags: %w", entry.Locator, err)
	}
	return nil
}

func verifyRestoreHash(entry *plugins.BackupMessage, digest [sha256.Size]byte, unverified, allowUnverified bool) error {
	if unverified && !allowUnverified {
		return fmt.Errorf("legacy backup message %q has no manifest hash; review a dry run and pass --allow-unverified to restore it", entry.Locator)
	}
	if !unverified && !equalHexDigest(entry.RawSHA256, digest[:]) {
		return fmt.Errorf("backup message %q failed SHA-256 verification", entry.Locator)
	}
	return nil
}

func equalHexDigest(want string, digest []byte) bool {
	return len(want) == hex.EncodedLen(len(digest)) && hex.EncodeToString(digest) == want
}
