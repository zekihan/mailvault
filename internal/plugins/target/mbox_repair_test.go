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

type mboxRepairFixture struct {
	t                  *testing.T
	truncate           struct{ keep func(int) int }
	path               string
	mbox               *MboxTarget
	originalRaw        []byte
	originalLocator    string
	corruptBytes       []byte
	replacementRaw     []byte
	replacementLocator string
	listed             []plugins.BackupMessage
}

func (fixture *mboxRepairFixture) open(t *testing.T) {
	t.Helper()
	fixture.t = t
	fixture.path = filepath.Join(t.TempDir(), "archive.mbox")
	created, err := NewMboxTarget(map[string]any{"path": fixture.path})
	if err != nil {
		t.Fatal(err)
	}
	fixture.mbox = mustAssert[*MboxTarget](t, created)
	if err := fixture.mbox.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func (fixture *mboxRepairFixture) writeOriginal(t *testing.T) {
	t.Helper()
	fixture.originalRaw = []byte("Message-ID: <original@example.invalid>\r\n\r\noriginal message\r\n")
	var err error
	fixture.originalLocator, err = fixture.mbox.WriteBackupMessage(t.Context(), &plugins.Message{Raw: fixture.originalRaw})
	if err != nil {
		t.Fatal(err)
	}
}

func (fixture *mboxRepairFixture) truncateOriginal(t *testing.T) {
	t.Helper()
	originalBytes, err := os.ReadFile(fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	kept := fixture.truncate.keep(len(originalBytes))
	if err := os.Truncate(fixture.path, int64(kept)); err != nil {
		t.Fatal(err)
	}
	fixture.corruptBytes, err = os.ReadFile(fixture.path)
	if err != nil {
		t.Fatal(err)
	}
}

func (fixture *mboxRepairFixture) writeReplacement(t *testing.T) {
	t.Helper()
	fixture.replacementRaw = []byte("Message-ID: <replacement@example.invalid>\r\n\r\nrepaired message\r\n")
	var err error
	fixture.replacementLocator, err = fixture.mbox.WriteRepairMessage(t.Context(), &plugins.Message{Raw: fixture.replacementRaw})
	if err != nil {
		t.Fatalf("repair append failed: %v", err)
	}
}

func (fixture *mboxRepairFixture) checkOffset(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, fixture.corruptBytes) {
		t.Fatal("repair changed the retained corrupt bytes")
	}
	wantOffset := int64(len(fixture.corruptBytes))
	if len(fixture.corruptBytes) > 0 && fixture.corruptBytes[len(fixture.corruptBytes)-1] != '\n' {
		wantOffset++
	}
	parts := strings.Split(fixture.replacementLocator, ":")
	if len(parts) != 4 {
		t.Fatalf("invalid replacement locator %q", fixture.replacementLocator)
	}
	offset, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || offset != wantOffset {
		t.Fatalf("replacement offset = %q, %v; want %d", parts[0], err, wantOffset)
	}
}

func (fixture *mboxRepairFixture) listAfterRepair(t *testing.T) {
	t.Helper()
	var err error
	fixture.listed, err = fixture.mbox.ListBackupMessages(t.Context())
	if err != nil {
		t.Fatalf("list after repair: %v", err)
	}
	var found bool
	for i := range fixture.listed {
		if fixture.listed[i].Locator == fixture.replacementLocator {
			found = true
		}
	}
	if !found {
		t.Fatalf("replacement locator %q missing from listing: %+v", fixture.replacementLocator, fixture.listed)
	}
}

func (fixture *mboxRepairFixture) checkMerged(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	manifest := []plugins.BackupMessage{{
		Target: fixture.mbox.Name(), Locator: fixture.replacementLocator,
		RawSHA256: dedup.ComputeContentHash(fixture.replacementRaw),
	}}
	merged, err := restore.MergeBackupMessagesCanonical(fixture.listed, manifest, fixture.mbox.CanonicalBackupLocator)
	if err != nil {
		t.Fatalf("merge after repair: %v", err)
	}
	mergedReplacement := false
	for i := range merged {
		if merged[i].Locator != fixture.replacementLocator {
			continue
		}
		mergedReplacement = true
		fixture.checkMergedEntry(ctx, t, &merged[i])
	}
	if !mergedReplacement {
		t.Fatalf("restore merge omitted replacement %s (original locator %s): %+v", fixture.replacementLocator, fixture.originalLocator, merged)
	}
}

func (fixture *mboxRepairFixture) checkMergedEntry(ctx context.Context, t *testing.T, entry *plugins.BackupMessage) {
	t.Helper()
	stored, err := fixture.mbox.ReadBackupMessage(ctx, entry.Locator)
	if err != nil {
		t.Fatalf("read merged replacement: %v", err)
	}
	if !bytes.Equal(stored.Raw, fixture.replacementRaw) {
		t.Fatalf("replacement bytes = %q, want %q", stored.Raw, fixture.replacementRaw)
	}
}

func TestMboxRepairAppendsAtRecordBoundary(t *testing.T) {
	for _, truncate := range []struct {
		name string
		keep func(int) int
	}{
		{name: "final newlines removed", keep: func(size int) int { return size - 2 }},
		{name: "whole record removed", keep: func(int) int { return 0 }},
	} {
		t.Run(truncate.name, func(t *testing.T) {
			fixture := &mboxRepairFixture{}
			fixture.truncate.keep = truncate.keep
			fixture.open(t)
			defer fixture.mbox.Close()
			fixture.writeOriginal(t)
			fixture.truncateOriginal(t)
			fixture.writeReplacement(t)
			fixture.checkOffset(t)
			fixture.listAfterRepair(t)
			fixture.checkMerged(t)
		})
	}
}

func TestMboxRepairLocatorCoversOnlyReplacementRecord(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.mbox")
	target, err := NewMboxTarget(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	mbox := mustAssert[*MboxTarget](t, target)
	if err := mbox.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	defer mbox.Close()
	if _, err := mbox.WriteBackupMessage(ctx, &plugins.Message{Raw: []byte("Subject: old\n\nold body\n")}); err != nil { //nolint:dupword // fixture message content, not prose
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: new\n\nnew body\n") //nolint:dupword // fixture message content, not prose
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
