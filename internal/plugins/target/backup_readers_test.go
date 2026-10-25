package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMaildirBackupLocatorSurvivesClientFlagRename(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	target, err := NewMaildirTarget(map[string]interface{}{"path": root})
	if err != nil {
		t.Fatal(err)
	}
	maildir := target.(*MaildirTarget)
	if err := maildir.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: fixture\r\n\r\nbody\r\n")
	locator, err := maildir.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, filepath.FromSlash(locator))
	newDir := filepath.Join(root, "cur")
	newName := filepath.Base(old)
	if runtime.GOOS != "windows" {
		newName += ":2,S"
	}
	newPath := filepath.Join(newDir, newName)
	if err := os.Rename(old, newPath); err != nil {
		t.Fatal(err)
	}
	stored, err := maildir.ReadBackupMessage(ctx, locator)
	if err != nil {
		t.Fatal(err)
	}
	if dedup.ComputeContentHash(stored.Raw) != dedup.ComputeContentHash(raw) {
		t.Fatal("renamed Maildir message did not verify")
	}
}

func TestMboxBackupLocatorPreservesExactRawBytes(t *testing.T) {
	ctx := context.Background()
	target, err := NewMboxTarget(map[string]interface{}{"path": filepath.Join(t.TempDir(), "archive.mbox")})
	if err != nil {
		t.Fatal(err)
	}
	mbox := target.(*MboxTarget)
	if err := mbox.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: fixture\r\n\r\nbody From line\r\n>From escaped\r\n>>From double escaped without final newline")
	locator, err := mbox.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := mbox.ReadBackupMessage(ctx, locator)
	if err != nil {
		t.Fatal(err)
	}
	if dedup.ComputeContentHash(stored.Raw) != dedup.ComputeContentHash(raw) {
		t.Fatalf("mbox roundtrip changed raw bytes: %q", stored.Raw)
	}
}

func TestMboxMalformedHeadersListAsOneMessage(t *testing.T) {
	for name, newline := range map[string]string{"LF": "\n", "CRLF": "\r\n"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			mbox := &MboxTarget{path: filepath.Join(t.TempDir(), "archive.mbox"), create: true}
			if err := mbox.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			defer mbox.Close()
			raw := []byte(strings.Join([]string{
				"From first line", "Subject: fixture", "From forged separator",
				">From quoted once", ">>From quoted twice", "X-Fixture: value", "",
				"From body", ">From quoted body", "",
			}, newline))
			locator, err := mbox.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
			if err != nil {
				t.Fatal(err)
			}
			entries, err := mbox.ListBackupMessages(ctx)
			if err != nil || len(entries) != 1 {
				t.Fatalf("listed %d messages, want one: %v", len(entries), err)
			}
			if entries[0].Locator != locator {
				t.Fatalf("listed locator = %q, want writer locator %q", entries[0].Locator, locator)
			}
			stored, err := mbox.ReadBackupMessage(ctx, entries[0].Locator)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(stored.Raw, raw) {
				t.Fatalf("roundtrip changed raw bytes: got %q, want %q", stored.Raw, raw)
			}
		})
	}
}

func TestMboxListingStreamsLongLinesAndMboxrdEscapes(t *testing.T) {
	ctx := context.Background()
	target, err := NewMboxTarget(map[string]interface{}{"path": filepath.Join(t.TempDir(), "archive.mbox")})
	if err != nil {
		t.Fatal(err)
	}
	mbox := target.(*MboxTarget)
	if err := mbox.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	defer mbox.Close()
	raw := append([]byte("Subject: fixture\n\n"), bytes.Repeat([]byte("x"), 128*1024)...)
	raw = append(raw, []byte("\nFrom body\n>From escaped\n>>From double\n")...)
	raw = append(raw, bytes.Repeat([]byte(">"), 32*1024-2)...)
	raw = append(raw, []byte("From crossing chunk boundary\n")...)
	locator, err := mbox.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := mbox.ListBackupMessages(ctx)
	if err != nil || len(entries) != 1 {
		t.Fatalf("listed %d messages, %v", len(entries), err)
	}
	if entries[0].Locator != locator {
		t.Fatalf("listed locator = %q, want exact writer locator %q", entries[0].Locator, locator)
	}
	stored, err := mbox.ReadBackupMessage(ctx, entries[0].Locator)
	if err != nil {
		t.Fatalf("read listed record: %v", err)
	}
	if !bytes.Equal(stored.Raw, raw) {
		t.Fatalf("read listed record = %q, want original bytes", stored.Raw)
	}
}

func TestMboxRecordLengthStreamingMatchesMboxrdUnescape(t *testing.T) {
	for split := 0; split <= len("From "); split++ {
		t.Run(fmt.Sprintf("split_%d", split), func(t *testing.T) {
			encoded := append(bytes.Repeat([]byte(">"), 32*1024-split), []byte("From cross-chunk\n")...)
			encoded = append(encoded, []byte("\n>FromX\n")...)
			record := append([]byte("From sender\n"), encoded...)
			record = append(record, '\n')
			path := filepath.Join(t.TempDir(), "record.mbox")
			if err := os.WriteFile(path, record, 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			encodedLength, decodedLength, hasSeparatorNewline, err := mboxRecordLengths(context.Background(), file, 0, int64(len(record)))
			if err != nil || !hasSeparatorNewline {
				t.Fatalf("record lengths error = %v, separator newline = %v", err, hasSeparatorNewline)
			}
			if encodedLength != int64(len(encoded)) {
				t.Fatalf("encoded length = %d, want %d", encodedLength, len(encoded))
			}
			wantDecoded := len(unescapeMboxrd(encoded))
			if decodedLength != int64(wantDecoded) {
				t.Fatalf("decoded length = %d, want %d", decodedLength, wantDecoded)
			}
		})
	}
}

func TestMboxBackupReaderSeparatesMessagesAndRejectsOutOfBoundsLocator(t *testing.T) {
	ctx := context.Background()
	target, err := NewMboxTarget(map[string]interface{}{"path": filepath.Join(t.TempDir(), "archive.mbox")})
	if err != nil {
		t.Fatal(err)
	}
	mbox := target.(*MboxTarget)
	if err := mbox.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	wants := [][]byte{
		[]byte("Message-ID: <one@example.invalid>\r\n\r\nfirst\r\nFrom separator\r\n"),
		[]byte("Message-ID: <two@example.invalid>\r\n\r\nsecond with final newline\r\n"),
	}
	for _, raw := range wants {
		if _, err := mbox.WriteBackupMessage(ctx, &plugins.Message{Raw: raw}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := mbox.ListBackupMessages(ctx)
	if err != nil || len(entries) != len(wants) {
		t.Fatalf("listed %d messages, %v", len(entries), err)
	}
	for i, entry := range entries {
		stored, err := mbox.ReadBackupMessage(ctx, entry.Locator)
		if err != nil || dedup.ComputeContentHash(stored.Raw) != dedup.ComputeContentHash(wants[i]) {
			t.Fatalf("message %d read = %v, %v", i, stored, err)
		}
	}
	if _, err := mbox.ReadBackupMessage(ctx, "0:999999:1:1"); !errors.Is(err, plugins.ErrBackupMessageCorrupt) {
		t.Fatalf("out-of-bounds locator error = %v", err)
	}
}

func TestMboxListingRejectsMissingInitialSeparator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.mbox")
	if err := os.WriteFile(path, []byte("Subject: damaged\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := NewMboxTarget(map[string]interface{}{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.(*MboxTarget).ListBackupMessages(context.Background()); !errors.Is(err, plugins.ErrBackupMessageCorrupt) {
		t.Fatalf("listing malformed mbox error = %v, want corruption", err)
	}
}

func TestMboxListingBoundsOversizedRecordBeforeAllocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.mbox")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("From sender\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	target, err := NewMboxTarget(map[string]interface{}{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.(*MboxTarget).listBackupMessages(context.Background(), 128); !errors.Is(err, plugins.ErrBackupMessageCorrupt) {
		t.Fatalf("listing oversized record error = %v, want corruption", err)
	}
}
