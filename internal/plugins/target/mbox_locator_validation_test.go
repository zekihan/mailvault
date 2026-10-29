package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/restore"
)

func TestMboxReaderRejectsLocatorsThatDiscardPayload(t *testing.T) {
	separator := "From sender\n"
	payload := "Subject: synthetic\n\nbody\n"
	record := separator + payload + "\n"
	for _, tc := range []struct {
		name         string
		data         string
		encoded, raw int
	}{
		{"short encoded extent", record, len(payload) - 2, len(payload) - 2},
		{"short raw extent", record, len(payload), len(payload) - 2},
		{"zero raw extent", record, len(payload), 0},
		{"missing final framing newline", separator + payload, len(payload) - 1, len(payload) - 1},
		{"one byte payload is not padding", separator + "x\n", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "archive.mbox")
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			mbox := &MboxTarget{path: path}
			locator := fmt.Sprintf("0:%d:%d:%d", len(tc.data), tc.encoded, tc.raw)
			msg, err := mbox.ReadBackupMessage(context.Background(), locator)
			if !errors.Is(err, plugins.ErrBackupMessageCorrupt) {
				t.Fatalf("read = %+v, %v; want corruption", msg, err)
			}
		})
	}
}

func TestMboxReaderRejectsIncompleteListedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.mbox")
	if err := os.WriteFile(path, []byte("From sender\nSubject: synthetic\n\ntruncated body"), 0o600); err != nil {
		t.Fatal(err)
	}
	mbox := &MboxTarget{path: path}
	entries, err := mbox.ListBackupMessages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	msg, err := mbox.ReadBackupMessage(context.Background(), entries[0].Locator)
	if !errors.Is(err, plugins.ErrBackupMessageCorrupt) {
		t.Fatalf("read = %+v, %v; want corruption", msg, err)
	}
	result, err := restore.Run(context.Background(), mbox, nil, nil, restore.Options{
		BackupTarget: "archive", DestinationSource: "synthetic", DestinationFolder: "restore", DryRun: true,
	})
	if !errors.Is(err, plugins.ErrBackupMessageCorrupt) || result.Planned != 0 {
		t.Fatalf("restore plan = %+v, %v; want corruption before planning any message", result, err)
	}
}

func TestMboxReaderPreservesValidRawLengthVariants(t *testing.T) {
	ctx := context.Background()
	for _, raw := range [][]byte{nil, []byte("x"), []byte("Subject: synthetic\n\nbody\n"), []byte("From escaped\n>From quoted")} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			mbox := &MboxTarget{path: filepath.Join(t.TempDir(), "archive.mbox"), create: true}
			if err := mbox.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			defer mbox.Close()
			locator, err := mbox.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
			if err != nil {
				t.Fatal(err)
			}
			msg, err := mbox.ReadBackupMessage(ctx, locator)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(msg.Raw, raw) {
				t.Fatalf("raw = %q, want %q", msg.Raw, raw)
			}
		})
	}
}
