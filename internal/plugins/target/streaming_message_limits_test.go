package target

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

type streamingLimitsFixture struct {
	kind      string
	raw       []byte
	oversized []byte
	value     plugins.Target
	valid     int
}

func (fixture *streamingLimitsFixture) open(t *testing.T) {
	t.Helper()
	values := map[string]any{"path": filepath.Join(physicalTempDir(t), "backup"), "max_message_bytes": len(fixture.oversized), "max_listing_entries": 10, "max_listing_locator_bytes": 4096}
	var err error
	if fixture.kind == "maildir" {
		fixture.value, err = NewMaildirTarget(values)
	} else {
		fixture.value, err = NewMboxTarget(values)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.value.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func (fixture *streamingLimitsFixture) writeMessages(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	writer := mustAssert[plugins.BackupWriter](t, fixture.value)
	for _, data := range [][]byte{fixture.raw, fixture.oversized} {
		if _, err := writer.WriteBackupMessage(ctx, &plugins.Message{Raw: data}); err != nil {
			t.Fatal(err)
		}
	}
}

func (fixture *streamingLimitsFixture) tightenMessageLimit() {
	switch target := fixture.value.(type) {
	case *MaildirTarget:
		target.maxMessageBytes = int64(len(fixture.raw))
	case *MboxTarget:
		target.maxMessageBytes = int64(len(fixture.raw))
	}
}

func (fixture *streamingLimitsFixture) walk(t *testing.T) error {
	t.Helper()
	ctx := t.Context()
	reader := mustAssert[plugins.TargetBackupReader](t, fixture.value)
	valid := 0
	err := plugins.WalkBackupMessages(ctx, reader, func(entry plugins.BackupMessage) error {
		message, err := reader.ReadBackupMessage(ctx, entry.Locator)
		if err != nil {
			return err
		}
		if !bytes.Equal(message.Raw, fixture.raw) {
			t.Fatal("stream returned unexpected body")
		}
		valid++
		return nil
	})
	fixture.valid = valid
	return err
}

func (fixture *streamingLimitsFixture) check(t *testing.T, err error) {
	t.Helper()
	// Mbox rejects decoded oversize records before emitting their locator;
	// Maildir rejects actual message bytes when the consumer reads the entry.
	if !errors.Is(err, plugins.ErrMessageTooLarge) || fixture.valid > 1 {
		t.Fatalf("message cap lost during streaming: valid=%d, error=%v", fixture.valid, err)
	}
	if fixture.kind == "mbox" && fixture.valid != 1 {
		t.Fatalf("valid leading mbox record was not processed: %d", fixture.valid)
	}
	if target, ok := fixture.value.(*MboxTarget); ok {
		entries, err := target.ListBackupMessages(t.Context())
		if !errors.Is(err, plugins.ErrMessageTooLarge) || entries != nil {
			t.Fatalf("partial mbox list escaped: %v, %v", entries, err)
		}
	}
}

func TestStreamingLocalBackupsRetainMessageAndListingLimits(t *testing.T) {
	raw := []byte("Subject: fixture\n\n" + strings.Repeat("x", 32) + "\n")
	oversized := append(bytes.Clone(raw), 'x', '\n')
	for _, kind := range []string{"maildir", "mbox"} {
		t.Run(kind, func(t *testing.T) {
			fixture := &streamingLimitsFixture{kind: kind, raw: raw, oversized: oversized}
			fixture.open(t)
			defer fixture.value.Close()
			fixture.writeMessages(t)
			fixture.tightenMessageLimit()
			err := fixture.walk(t)
			fixture.check(t, err)
		})
	}
}

func TestStreamingS3ReadRetainsActualMessageLimit(t *testing.T) {
	var listings, reads atomic.Int32
	target := newListingTestS3(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			listings.Add(1)
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken><Contents><Key>valid</Key></Contents><Contents><Key>oversized</Key></Contents></ListBucketResult>`)
			return
		}
		reads.Add(1)
		count := 64
		if strings.HasSuffix(r.URL.Path, "oversized") {
			count++
		}
		w.WriteHeader(http.StatusOK)
		mustAssert[http.Flusher](t, w).Flush()
		fmt.Fprint(w, strings.Repeat("x", count))
	}, backupListingLimits{entries: 10, locatorBytes: 4096})
	target.maxMessageBytes = 64
	valid := 0
	err := target.WalkBackupMessages(t.Context(), func(entry plugins.BackupMessage) error {
		message, err := target.ReadBackupMessage(t.Context(), entry.Locator)
		if err != nil {
			return err
		}
		if len(message.Raw) != 64 {
			t.Fatalf("body size=%d", len(message.Raw))
		}
		valid++
		return nil
	})
	if !errors.Is(err, plugins.ErrMessageTooLarge) || listings.Load() != 1 || reads.Load() != 2 || valid != 1 {
		t.Fatalf("stream cap lost: listings=%d, reads=%d, valid=%d, error=%v", listings.Load(), reads.Load(), valid, err)
	}
}
