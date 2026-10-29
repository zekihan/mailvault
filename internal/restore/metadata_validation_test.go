package restore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestRunRejectsInvalidArchiveMetadataBeforeDestinationMutation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		date  string
	}{
		{name: "date", date: "not-a-date"},
		{name: "keyword", flags: []string{"bad flag"}},
		{name: "system flag", flags: []string{`\Unknown`}},
	} {
		for _, fromManifest := range []bool{false, true} {
			for _, dryRun := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/manifest=%t/dry=%t", tc.name, fromManifest, dryRun), func(t *testing.T) {
					raw := []byte("Subject: synthetic\r\n\r\nfixture\r\n")
					digest := fmt.Sprintf("%x", sha256.Sum256(raw))
					entries := []plugins.BackupMessage{{Locator: "valid", RawSHA256: digest}, {Locator: "invalid", RawSHA256: digest}}
					messages := []*plugins.Message{{Raw: raw}, {Raw: raw}}
					if fromManifest {
						entries[1].Flags, entries[1].InternalDate = tc.flags, tc.date
					} else {
						messages[1].Flags, messages[1].InternalDate = tc.flags, tc.date
					}
					reader := metadataReader{entries: entries, messages: messages}
					appender := &fixtureAppender{}
					receipts := newFixtureReceipts()
					_, err := Run(context.Background(), reader, appender, receipts, Options{BackupTarget: "archive", DestinationSource: "account", DestinationFolder: "restore", DryRun: dryRun})
					if err == nil {
						t.Fatal("invalid metadata passed preflight")
					}
					if appender.ensured != 0 || appender.appended != 0 || len(receipts.jobs) != 0 || len(receipts.preparing) != 0 || len(receipts.pending) != 0 {
						t.Fatalf("invalid metadata caused destination/state mutation: appender=%+v receipts=%+v", appender, receipts)
					}
				})
			}
		}
	}
}

type metadataReader struct {
	entries  []plugins.BackupMessage
	messages []*plugins.Message
}

func (r metadataReader) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	return r.entries, nil
}
func (r metadataReader) ReadBackupMessage(_ context.Context, locator string) (*plugins.Message, error) {
	for i, entry := range r.entries {
		if entry.Locator == locator {
			copy := *r.messages[i]
			return &copy, nil
		}
	}
	return nil, fmt.Errorf("unknown fixture locator %q", locator)
}

func TestRunValidatesEffectiveMetadataAndPreservesReaderMessage(t *testing.T) {
	raw := []byte("Subject: synthetic\r\n\r\nfixture\r\n")
	entry := plugins.BackupMessage{Locator: "object", RawSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), Flags: []string{"invalid flag"}, InternalDate: "invalid-date", UID: "17"}
	message := &plugins.Message{Raw: raw, Flags: []string{`\sEeN`, `\Recent`, "$Label1"}, InternalDate: "2026-01-02T03:04:05Z"}
	appender := &fixtureAppender{}
	result, err := Run(context.Background(), fixtureReader{entries: []plugins.BackupMessage{entry}, message: message}, appender, newFixtureReceipts(), Options{BackupTarget: "archive", DestinationSource: "account", DestinationFolder: "restore"})
	if err != nil || result.Restored != 1 {
		t.Fatalf("valid reader metadata should override manifest: result=%+v err=%v", result, err)
	}
	if message.UID != "" {
		t.Fatalf("restore changed reader-owned metadata: %+v", message)
	}
	if appender.message.UID != "17" || appender.message.InternalDate != message.InternalDate || len(appender.message.Flags) != 3 {
		t.Fatalf("metadata merge changed: %+v", appender.message)
	}
}
