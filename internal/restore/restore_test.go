package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/state"
)

type fixtureReader struct {
	entries []plugins.BackupMessage
	message *plugins.Message
	err     error
}

func (r fixtureReader) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	return r.entries, nil
}

func (r fixtureReader) ReadBackupMessage(context.Context, string) (*plugins.Message, error) {
	return r.message, r.err
}

type fixtureAppender struct {
	mailbox       string
	message       *plugins.Message
	ensured       int
	appended      int
	err           error
	appendErr     error
	allowExisting []bool
	cancel        context.CancelFunc
}

func (a *fixtureAppender) EnsureMailbox(_ context.Context, mailbox string, allowExisting bool) (bool, error) {
	a.mailbox = mailbox
	a.ensured++
	a.allowExisting = append(a.allowExisting, allowExisting)
	if a.err != nil {
		return false, a.err
	}
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	return !allowExisting, nil
}

func (a *fixtureAppender) AppendMessage(_ context.Context, mailbox string, message *plugins.Message) error {
	a.mailbox = mailbox
	a.message = message
	a.appended++
	return a.appendErr
}

type fixtureReceipts struct {
	values    map[state.RestoreReceipt]bool
	pending   map[state.RestoreReceipt]bool
	jobs      map[string]bool
	preparing map[string]bool
	cancel    context.CancelFunc
}

func (r fixtureReceipts) HasRestoreReceipt(_ context.Context, receipt state.RestoreReceipt) (bool, error) {
	return r.values[receipt], nil
}

func (r fixtureReceipts) HasPendingRestore(_ context.Context, source, folder, target string) (bool, error) {
	for receipt := range r.pending {
		if receipt.DestinationSource == source && receipt.DestinationFolder == folder && receipt.BackupTarget == target {
			return true, nil
		}
	}
	return false, nil
}

func (r fixtureReceipts) PendingRestoreLocators(_ context.Context, source, folder, target string) ([]string, error) {
	var locators []string
	for receipt := range r.pending {
		if receipt.DestinationSource == source && receipt.DestinationFolder == folder && receipt.BackupTarget == target {
			locators = append(locators, receipt.Locator+" [source="+receipt.Source+"]")
		}
	}
	return locators, nil
}

func (r fixtureReceipts) BeginRestoreReceipt(_ context.Context, receipt state.RestoreReceipt) error {
	r.pending[receipt] = true
	return nil
}

func fixtureJobKey(source, folder, target string) string {
	return source + "\x00" + folder + "\x00" + target
}

func (r fixtureReceipts) BeginRestoreJob(_ context.Context, source, folder, target string) (bool, error) {
	key := fixtureJobKey(source, folder, target)
	if r.preparing[key] {
		return false, state.ErrRestoreJobUncertain
	}
	if r.jobs[key] {
		return true, nil
	}
	r.preparing[key] = true
	return false, nil
}

func (r fixtureReceipts) ActivateRestoreJob(_ context.Context, source, folder, target string) error {
	key := fixtureJobKey(source, folder, target)
	delete(r.preparing, key)
	r.jobs[key] = true
	return nil
}

func (r fixtureReceipts) AbandonRestoreJob(_ context.Context, source, folder, target string) error {
	delete(r.preparing, fixtureJobKey(source, folder, target))
	return nil
}

func (r fixtureReceipts) MarkRestoreReceipt(_ context.Context, receipt state.RestoreReceipt) error {
	r.values[receipt] = true
	delete(r.pending, receipt)
	if r.cancel != nil {
		r.cancel()
	}
	return nil
}

func newFixtureReceipts() fixtureReceipts {
	return fixtureReceipts{
		values: make(map[state.RestoreReceipt]bool), pending: make(map[state.RestoreReceipt]bool),
		jobs: make(map[string]bool), preparing: make(map[string]bool),
	}
}

func TestRunRestoresVerifiedMessageAndMetadata(t *testing.T) {
	raw := []byte("From: fixture@example.invalid\r\n\r\nhello\r\n")
	digest := sha256.Sum256(raw)
	entry := plugins.BackupMessage{Target: "archive", Locator: "cur/message", RawSHA256: hex.EncodeToString(digest[:]),
		Scope: "scope", Folder: "Inbox", UID: "17", Flags: []string{"\\Seen"}, InternalDate: "2026-01-02T03:04:05Z"}
	reader := fixtureReader{entries: []plugins.BackupMessage{entry}, message: &plugins.Message{Raw: raw}}
	appender := &fixtureAppender{}
	receipts := newFixtureReceipts()
	result, err := Run(context.Background(), reader, appender, receipts, Options{BackupTarget: "archive", DestinationSource: "account", DestinationFolder: "Mailvault Restore"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Restored != 1 || result.Skipped != 0 || appender.ensured != 1 || appender.appended != 1 {
		t.Fatalf("unexpected result: %+v appender=%+v", result, appender)
	}
	if appender.message.UID != "17" || appender.message.Folder != "Inbox" || appender.message.UIDScope != "scope" || len(appender.message.Flags) != 1 || appender.message.InternalDate != entry.InternalDate {
		t.Fatalf("source metadata was not preserved: %+v", appender.message)
	}
}

func TestRunRejectsChangedBackupBytesBeforeAppend(t *testing.T) {
	raw := []byte("changed")
	digest := sha256.Sum256([]byte("original"))
	entry := plugins.BackupMessage{Target: "archive", Locator: "object", RawSHA256: hex.EncodeToString(digest[:])}
	appender := &fixtureAppender{}
	_, err := Run(context.Background(), fixtureReader{entries: []plugins.BackupMessage{entry}, message: &plugins.Message{Raw: raw}}, appender, newFixtureReceipts(), Options{BackupTarget: "archive", DestinationSource: "account", DestinationFolder: "Mailvault Restore"})
	if err == nil || appender.appended != 0 {
		t.Fatalf("changed backup was not rejected before append: err=%v appends=%d", err, appender.appended)
	}
}

func TestRunValidatesEveryMessageBeforeCreatingMailbox(t *testing.T) {
	raw := []byte("first")
	digest := sha256.Sum256(raw)
	entries := []plugins.BackupMessage{
		{Locator: "valid", RawSHA256: hex.EncodeToString(digest[:])},
		{Locator: "corrupt", RawSHA256: hex.EncodeToString(digest[:])},
	}
	reader := sequenceReader{entries: entries, messages: []*plugins.Message{{Raw: raw}, {Raw: []byte("corrupt")}}}
	appender := &fixtureAppender{}
	receipts := newFixtureReceipts()
	_, err := Run(context.Background(), &reader, appender, receipts, Options{BackupTarget: "archive", DestinationSource: "account", DestinationFolder: "restore"})
	if err == nil || appender.ensured != 0 || appender.appended != 0 || len(receipts.jobs) != 0 {
		t.Fatalf("corrupt backup caused destination or state mutation: err=%v appender=%+v jobs=%d", err, appender, len(receipts.jobs))
	}
}

func TestRunReceiptSkipsCompletedMessage(t *testing.T) {
	digest := sha256.Sum256([]byte("content"))
	entry := plugins.BackupMessage{Locator: "object", RawSHA256: hex.EncodeToString(digest[:])}
	receipt := state.RestoreReceipt{DestinationSource: "account", DestinationFolder: "restore", BackupTarget: "archive", Locator: "object", RawSHA256: entry.RawSHA256}
	appender := &fixtureAppender{}
	receipts := newFixtureReceipts()
	receipts.values[receipt] = true
	receipts.jobs[fixtureJobKey("account", "restore", "archive")] = true
	result, err := Run(context.Background(), fixtureReader{entries: []plugins.BackupMessage{entry}, message: &plugins.Message{Raw: []byte("content")}}, appender, receipts, Options{BackupTarget: "archive", DestinationSource: "account", DestinationFolder: "restore"})
	if err != nil || result.Skipped != 1 || appender.appended != 0 {
		t.Fatalf("receipt did not skip append: result=%+v err=%v", result, err)
	}
}

func TestRunDoesNotRecordReceiptWhenAppendFails(t *testing.T) {
	digest := sha256.Sum256([]byte("content"))
	entry := plugins.BackupMessage{Locator: "object", RawSHA256: hex.EncodeToString(digest[:])}
	appender := &fixtureAppender{appendErr: errors.New("fixture append failed")}
	receipts := newFixtureReceipts()
	_, err := Run(context.Background(), fixtureReader{entries: []plugins.BackupMessage{entry}, message: &plugins.Message{Raw: []byte("content")}}, appender, receipts, Options{BackupTarget: "archive", DestinationSource: "account", DestinationFolder: "restore"})
	if err == nil || len(receipts.values) != 0 || len(receipts.pending) != 1 {
		t.Fatalf("failed append did not preserve an uncertainty marker: err=%v completed=%d pending=%d", err, len(receipts.values), len(receipts.pending))
	}
}

func TestRunResumesAfterCompletedMessage(t *testing.T) {
	raw := []byte("content")
	digest := sha256.Sum256(raw)
	entries := []plugins.BackupMessage{
		{Locator: "one", RawSHA256: hex.EncodeToString(digest[:])},
		{Locator: "two", RawSHA256: hex.EncodeToString(digest[:])},
	}
	ctx, cancel := context.WithCancel(context.Background())
	receipts := newFixtureReceipts()
	receipts.cancel = cancel
	appender := &fixtureAppender{}
	reader := fixtureReader{entries: entries, message: &plugins.Message{Raw: raw}}
	options := Options{BackupTarget: "archive", DestinationSource: "account", DestinationFolder: "restore"}
	if _, err := Run(ctx, reader, appender, receipts, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted run error = %v, want context canceled", err)
	}
	if len(receipts.values) != 1 || len(receipts.pending) != 0 || appender.appended != 1 {
		t.Fatalf("first message state: completed=%d pending=%d appends=%d", len(receipts.values), len(receipts.pending), appender.appended)
	}

	resumed := &fixtureAppender{}
	result, err := Run(context.Background(), reader, resumed, receipts, options)
	if err != nil || result.Restored != 1 || result.Skipped != 1 || resumed.appended != 1 || len(resumed.allowExisting) != 1 || !resumed.allowExisting[0] {
		t.Fatalf("resume result=%+v appender=%+v err=%v", result, resumed, err)
	}
}

func TestRunResumesAfterMailboxCreationAndCancellation(t *testing.T) {
	raw := []byte("content")
	digest := sha256.Sum256(raw)
	entry := plugins.BackupMessage{Locator: "one", RawSHA256: hex.EncodeToString(digest[:])}
	ctx, cancel := context.WithCancel(context.Background())
	receipts := newFixtureReceipts()
	appender := &fixtureAppender{cancel: cancel}
	reader := fixtureReader{entries: []plugins.BackupMessage{entry}, message: &plugins.Message{Raw: raw}}
	options := Options{BackupTarget: "archive", DestinationSource: "account", DestinationFolder: "restore"}
	if _, err := Run(ctx, reader, appender, receipts, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled after mailbox creation: %v", err)
	}
	if !receipts.jobs[fixtureJobKey("account", "restore", "archive")] || appender.appended != 0 {
		t.Fatalf("restore job was not durable after mailbox creation: jobs=%v appends=%d", receipts.jobs, appender.appended)
	}
	resumed := &fixtureAppender{}
	if _, err := Run(context.Background(), reader, resumed, receipts, options); err != nil {
		t.Fatal(err)
	}
	if len(resumed.allowExisting) != 1 || !resumed.allowExisting[0] || resumed.appended != 1 {
		t.Fatalf("created mailbox was not resumed: appender=%+v", resumed)
	}
}

func TestRunRefusesExistingMailboxForNewJob(t *testing.T) {
	raw := []byte("content")
	digest := sha256.Sum256(raw)
	entry := plugins.BackupMessage{Locator: "one", RawSHA256: hex.EncodeToString(digest[:])}
	appender := &fixtureAppender{err: errors.Join(errors.New("destination mailbox already exists"), plugins.ErrMailboxExists)}
	_, err := Run(context.Background(), fixtureReader{entries: []plugins.BackupMessage{entry}, message: &plugins.Message{Raw: raw}}, appender, newFixtureReceipts(), Options{BackupTarget: "archive", DestinationSource: "account", DestinationFolder: "restore"})
	if err == nil || appender.appended != 0 {
		t.Fatalf("existing destination accepted for a new job: err=%v appends=%d", err, appender.appended)
	}
}

type sequenceReader struct {
	entries  []plugins.BackupMessage
	messages []*plugins.Message
	read     int
}

func (r *sequenceReader) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	return r.entries, nil
}

func (r *sequenceReader) ReadBackupMessage(context.Context, string) (*plugins.Message, error) {
	message := r.messages[r.read]
	r.read++
	return message, nil
}
