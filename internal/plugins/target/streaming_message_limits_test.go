package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestStreamingLocalBackupsRetainMessageAndListingLimits(t *testing.T) {
	ctx := context.Background()
	raw := []byte("Subject: fixture\n\n" + strings.Repeat("x", 32) + "\n")
	oversized := append(bytes.Clone(raw), 'x', '\n')
	for _, kind := range []string{"maildir", "mbox"} {
		t.Run(kind, func(t *testing.T) {
			values := map[string]any{"path": filepath.Join(t.TempDir(), "backup"), "max_message_bytes": len(oversized), "max_listing_entries": 10, "max_listing_locator_bytes": 4096}
			var value plugins.Target
			var err error
			if kind == "maildir" {
				value, err = NewMaildirTarget(values)
			} else {
				value, err = NewMboxTarget(values)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer value.Close()
			if err := value.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			writer := value.(plugins.BackupWriter)
			for _, data := range [][]byte{raw, oversized} {
				if _, err := writer.WriteBackupMessage(ctx, &plugins.Message{Raw: data}); err != nil {
					t.Fatal(err)
				}
			}
			switch target := value.(type) {
			case *MaildirTarget:
				target.maxMessageBytes = int64(len(raw))
			case *MboxTarget:
				target.maxMessageBytes = int64(len(raw))
			}
			reader := value.(plugins.TargetBackupReader)
			valid := 0
			err = plugins.WalkBackupMessages(ctx, reader, func(entry plugins.BackupMessage) error {
				message, err := reader.ReadBackupMessage(ctx, entry.Locator)
				if err != nil {
					return err
				}
				if !bytes.Equal(message.Raw, raw) {
					t.Fatal("stream returned unexpected body")
				}
				valid++
				return nil
			})
			// Mbox rejects decoded oversize records before emitting their locator;
			// Maildir rejects actual message bytes when the consumer reads the entry.
			if !errors.Is(err, plugins.ErrMessageTooLarge) || valid > 1 {
				t.Fatalf("message cap lost during streaming: valid=%d, error=%v", valid, err)
			}
			if kind == "mbox" && valid != 1 {
				t.Fatalf("valid leading mbox record was not processed: %d", valid)
			}
			if target, ok := value.(*MboxTarget); ok {
				entries, err := target.ListBackupMessages(ctx)
				if !errors.Is(err, plugins.ErrMessageTooLarge) || entries != nil {
					t.Fatalf("partial mbox list escaped: %v, %v", entries, err)
				}
			}
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
		w.(http.Flusher).Flush()
		fmt.Fprint(w, strings.Repeat("x", count))
	}, backupListingLimits{entries: 10, locatorBytes: 4096})
	target.maxMessageBytes = 64
	valid := 0
	err := target.WalkBackupMessages(context.Background(), func(entry plugins.BackupMessage) error {
		message, err := target.ReadBackupMessage(context.Background(), entry.Locator)
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
