package target

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/restore"
)

func TestMboxRepairAppendsAtRecordBoundary(t *testing.T) {
	for _, truncate := range []struct {
		name string
		keep func(int) int
	}{
		{name: "final newlines removed", keep: func(size int) int { return size - 2 }},
		{name: "whole record removed", keep: func(int) int { return 0 }},
	} {
		t.Run(truncate.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "archive.mbox")
			created, err := NewMboxTarget(map[string]interface{}{"path": path})
			if err != nil {
				t.Fatal(err)
			}
			mbox := created.(*MboxTarget)
			if err := mbox.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			defer mbox.Close()

			originalRaw := []byte("Message-ID: <original@example.invalid>\r\n\r\noriginal message\r\n")
			originalLocator, err := mbox.WriteBackupMessage(ctx, &plugins.Message{Raw: originalRaw})
			if err != nil {
				t.Fatal(err)
			}
			originalBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			kept := truncate.keep(len(originalBytes))
			if err := os.Truncate(path, int64(kept)); err != nil {
				t.Fatal(err)
			}
			corruptBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			replacementRaw := []byte("Message-ID: <replacement@example.invalid>\r\n\r\nrepaired message\r\n")
			replacementLocator, err := mbox.WriteRepairMessage(ctx, &plugins.Message{Raw: replacementRaw})
			if err != nil {
				t.Fatalf("repair append failed: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(data, corruptBytes) {
				t.Fatal("repair changed the retained corrupt bytes")
			}
			wantOffset := int64(len(corruptBytes))
			if len(corruptBytes) > 0 && corruptBytes[len(corruptBytes)-1] != '\n' {
				wantOffset++
			}
			parts := strings.Split(replacementLocator, ":")
			if len(parts) != 4 {
				t.Fatalf("invalid replacement locator %q", replacementLocator)
			}
			offset, err := strconv.ParseInt(parts[0], 10, 64)
			if err != nil || offset != wantOffset {
				t.Fatalf("replacement offset = %q, %v; want %d", parts[0], err, wantOffset)
			}
			listed, err := mbox.ListBackupMessages(ctx)
			if err != nil {
				t.Fatalf("list after repair: %v", err)
			}
			var found bool
			for _, entry := range listed {
				if entry.Locator == replacementLocator {
					found = true
				}
			}
			if !found {
				t.Fatalf("replacement locator %q missing from listing: %+v", replacementLocator, listed)
			}

			manifest := []plugins.BackupMessage{{
				Target: mbox.Name(), Locator: replacementLocator,
				RawSHA256: dedup.ComputeContentHash(replacementRaw),
			}}
			merged, err := restore.MergeBackupMessagesCanonical(listed, manifest, mbox.CanonicalBackupLocator)
			if err != nil {
				t.Fatalf("merge after repair: %v", err)
			}
			mergedReplacement := false
			for _, entry := range merged {
				if entry.Locator != replacementLocator {
					continue
				}
				mergedReplacement = true
				stored, err := mbox.ReadBackupMessage(ctx, entry.Locator)
				if err != nil {
					t.Fatalf("read merged replacement: %v", err)
				}
				if !bytes.Equal(stored.Raw, replacementRaw) {
					t.Fatalf("replacement bytes = %q, want %q", stored.Raw, replacementRaw)
				}
			}
			if !mergedReplacement {
				t.Fatalf("restore merge omitted replacement %s (original locator %s): %+v", replacementLocator, originalLocator, merged)
			}
		})
	}
}

func TestMboxRepairLocatorCoversOnlyReplacementRecord(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "archive.mbox")
	target, err := NewMboxTarget(map[string]interface{}{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	mbox := target.(*MboxTarget)
	if err := mbox.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	defer mbox.Close()
	if _, err := mbox.WriteBackupMessage(ctx, &plugins.Message{Raw: []byte("Subject: old\n\nold body\n")}); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: new\n\nnew body\n")
	locator, err := mbox.WriteRepairMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	var offset, length int64
	if _, err := fmt.Sscanf(locator, "%d:%d", &offset, &length); err != nil {
		t.Fatal(err)
	}
	if offset != 0 {
		t.Fatalf("empty mailbox replacement offset = %d, want 0", offset)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if length != info.Size() {
		t.Fatalf("replacement length = %d, mailbox size = %d", length, info.Size())
	}
	stored, err := mbox.ReadBackupMessage(ctx, locator)
	if err != nil || !bytes.Equal(stored.Raw, raw) {
		t.Fatalf("read replacement = %q, %v", stored.Raw, err)
	}
}
