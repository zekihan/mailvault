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

type limitedTargetFixture struct {
	kind    string
	path    string
	raw     []byte
	target  plugins.Target
	locator string
	backup  interface {
		WriteBackupMessage(ctx context.Context, msg *plugins.Message) (string, error)
		WriteRepairMessage(ctx context.Context, msg *plugins.Message) (string, error)
		plugins.TargetBackupReader
	}
}

func (fixture *limitedTargetFixture) open(t *testing.T) {
	t.Helper()
	fixture.path = filepath.Join(physicalTempDir(t), "backup")
	values := map[string]any{"path": fixture.path, "max_message_bytes": int64(len(fixture.raw))}
	var err error
	if fixture.kind == "maildir" {
		fixture.target, err = NewMaildirTarget(values)
	} else {
		fixture.target, err = NewMboxTarget(values)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.target.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	fixture.backup = mustAssert[interface {
		WriteBackupMessage(ctx context.Context, msg *plugins.Message) (string, error)
		WriteRepairMessage(ctx context.Context, msg *plugins.Message) (string, error)
		plugins.TargetBackupReader
	}](t, fixture.target)
}

func (fixture *limitedTargetFixture) writeAndRoundtrip(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	var err error
	fixture.locator, err = fixture.backup.WriteBackupMessage(ctx, &plugins.Message{Raw: fixture.raw})
	if err != nil {
		t.Fatal(err)
	}
	got, err := fixture.backup.ReadBackupMessage(ctx, fixture.locator)
	if err != nil || !bytes.Equal(got.Raw, fixture.raw) {
		t.Fatalf("at-limit roundtrip: %v, %v", got, err)
	}
	fixture.checkListedRead(ctx, t)
}

func (fixture *limitedTargetFixture) checkListedRead(ctx context.Context, t *testing.T) {
	t.Helper()
	entries, err := fixture.backup.ListBackupMessages(ctx)
	if err != nil || len(entries) != 1 {
		t.Fatalf("verification list: %v, %v", entries, err)
	}
	got, err := fixture.backup.ReadBackupMessage(ctx, entries[0].Locator)
	if err != nil || !bytes.Equal(got.Raw, fixture.raw) {
		t.Fatalf("listed restore read: %v, %v", got, err)
	}
}

func (fixture *limitedTargetFixture) checkOversizedWrites(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	oversized := &plugins.Message{Raw: append(bytes.Clone(fixture.raw), 'x')}
	if _, err := fixture.backup.WriteBackupMessage(ctx, oversized); !errors.Is(err, plugins.ErrMessageTooLarge) {
		t.Fatalf("oversized write: %v", err)
	}
	if _, err := fixture.backup.WriteRepairMessage(ctx, oversized); !errors.Is(err, plugins.ErrMessageTooLarge) {
		t.Fatalf("oversized repair: %v", err)
	}
	fixture.checkOversizedRead(ctx, t, oversized)
}

func (fixture *limitedTargetFixture) checkOversizedRead(ctx context.Context, t *testing.T, oversized *plugins.Message) {
	t.Helper()
	switch target := fixture.target.(type) {
	case *MaildirTarget:
		if err := os.WriteFile(filepath.Join(fixture.path, filepath.FromSlash(fixture.locator)), oversized.Raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := target.ReadBackupMessage(ctx, fixture.locator); !errors.Is(err, plugins.ErrMessageTooLarge) {
			t.Fatalf("oversized backup read: %v", err)
		}
	case *MboxTarget:
		target.maxMessageBytes = int64(len(fixture.raw) - 1)
		if _, err := target.ReadBackupMessage(ctx, fixture.locator); !errors.Is(err, plugins.ErrMessageTooLarge) {
			t.Fatalf("oversized backup read: %v", err)
		}
	}
}

func TestTargetsApplySameLimitToWriteRepairAndRead(t *testing.T) {
	raw := []byte("Subject: fixture\r\n\r\nFrom escaped\r\n>From escaped again\r\n")
	for _, kind := range []string{"maildir", "mbox"} {
		t.Run(kind, func(t *testing.T) {
			fixture := &limitedTargetFixture{kind: kind, raw: raw}
			fixture.open(t)
			defer fixture.target.Close()
			fixture.writeAndRoundtrip(t)
			fixture.checkOversizedWrites(t)
		})
	}
}

func TestS3RejectsOversizeBeforePublication(t *testing.T) {
	target := &S3Target{maxMessageBytes: 4}
	for _, write := range []func(context.Context, *plugins.Message) (string, error){target.WriteBackupMessage, target.WriteRepairMessage} {
		if _, err := write(t.Context(), &plugins.Message{Raw: []byte("large")}); !errors.Is(err, plugins.ErrMessageTooLarge) {
			t.Fatalf("size check did not precede publication: %v", err)
		}
	}
}

func TestMboxReservesFinalNewlineWithinReadLimit(t *testing.T) {
	raw := []byte("body")
	value, err := NewMboxTarget(map[string]any{"path": filepath.Join(physicalTempDir(t), "archive.mbox"), "max_message_bytes": len(raw)})
	if err != nil {
		t.Fatal(err)
	}
	target := mustAssert[*MboxTarget](t, value)
	if err := target.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := target.WriteBackupMessage(t.Context(), &plugins.Message{Raw: raw}); !errors.Is(err, plugins.ErrMessageTooLarge) {
		t.Fatalf("unreadable at-limit mbox accepted: %v", err)
	}
	target.maxMessageBytes++
	if _, err := target.WriteBackupMessage(t.Context(), &plugins.Message{Raw: raw}); err != nil {
		t.Fatal(err)
	}
	entries, err := target.ListBackupMessages(t.Context())
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v, %v", entries, err)
	}
	if _, err := target.ReadBackupMessage(t.Context(), entries[0].Locator); err != nil {
		t.Fatal(err)
	}
}
