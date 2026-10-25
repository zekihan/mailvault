package target

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestTargetsApplySameLimitToWriteRepairAndRead(t *testing.T) {
	ctx := context.Background()
	raw := []byte("Subject: fixture\r\n\r\nFrom escaped\r\n>From escaped again\r\n")
	for _, kind := range []string{"maildir", "mbox"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "backup")
			values := map[string]interface{}{"path": path, "max_message_bytes": int64(len(raw))}
			var target plugins.Target
			var err error
			if kind == "maildir" {
				target, err = NewMaildirTarget(values)
			} else {
				target, err = NewMboxTarget(values)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			if err := target.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			backup := target.(interface {
				WriteBackupMessage(context.Context, *plugins.Message) (string, error)
				WriteRepairMessage(context.Context, *plugins.Message) (string, error)
				plugins.TargetBackupReader
			})
			locator, err := backup.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
			if err != nil {
				t.Fatal(err)
			}
			got, err := backup.ReadBackupMessage(ctx, locator)
			if err != nil || !bytes.Equal(got.Raw, raw) {
				t.Fatalf("at-limit roundtrip: %v, %v", got, err)
			}
			entries, err := backup.ListBackupMessages(ctx)
			if err != nil || len(entries) != 1 {
				t.Fatalf("verification list: %v, %v", entries, err)
			}
			got, err = backup.ReadBackupMessage(ctx, entries[0].Locator)
			if err != nil || !bytes.Equal(got.Raw, raw) {
				t.Fatalf("listed restore read: %v, %v", got, err)
			}
			oversized := &plugins.Message{Raw: append(bytes.Clone(raw), 'x')}
			if _, err := backup.WriteBackupMessage(ctx, oversized); !errors.Is(err, plugins.ErrMessageTooLarge) {
				t.Fatalf("oversized write: %v", err)
			}
			if _, err := backup.WriteRepairMessage(ctx, oversized); !errors.Is(err, plugins.ErrMessageTooLarge) {
				t.Fatalf("oversized repair: %v", err)
			}
			switch target := target.(type) {
			case *MaildirTarget:
				if err := os.WriteFile(filepath.Join(path, filepath.FromSlash(locator)), oversized.Raw, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := target.ReadBackupMessage(ctx, locator); !errors.Is(err, plugins.ErrMessageTooLarge) {
					t.Fatalf("oversized backup read: %v", err)
				}
			case *MboxTarget:
				target.maxMessageBytes = int64(len(raw) - 1)
				if _, err := target.ReadBackupMessage(ctx, locator); !errors.Is(err, plugins.ErrMessageTooLarge) {
					t.Fatalf("oversized backup read: %v", err)
				}
			}
		})
	}
}

func TestS3RejectsOversizeBeforePublication(t *testing.T) {
	target := &S3Target{maxMessageBytes: 4}
	for _, write := range []func(context.Context, *plugins.Message) (string, error){target.WriteBackupMessage, target.WriteRepairMessage} {
		if _, err := write(context.Background(), &plugins.Message{Raw: []byte("large")}); !errors.Is(err, plugins.ErrMessageTooLarge) {
			t.Fatalf("size check did not precede publication: %v", err)
		}
	}
}

func TestMboxReservesFinalNewlineWithinReadLimit(t *testing.T) {
	raw := []byte("body")
	value, err := NewMboxTarget(map[string]interface{}{"path": filepath.Join(t.TempDir(), "archive.mbox"), "max_message_bytes": len(raw)})
	if err != nil {
		t.Fatal(err)
	}
	target := value.(*MboxTarget)
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := target.WriteBackupMessage(context.Background(), &plugins.Message{Raw: raw}); !errors.Is(err, plugins.ErrMessageTooLarge) {
		t.Fatalf("unreadable at-limit mbox accepted: %v", err)
	}
	target.maxMessageBytes++
	if _, err := target.WriteBackupMessage(context.Background(), &plugins.Message{Raw: raw}); err != nil {
		t.Fatal(err)
	}
	entries, err := target.ListBackupMessages(context.Background())
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v, %v", entries, err)
	}
	if _, err := target.ReadBackupMessage(context.Background(), entries[0].Locator); err != nil {
		t.Fatal(err)
	}
}
