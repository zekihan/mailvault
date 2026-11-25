package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/state"
)

func setupCorruptMaildir(t *testing.T) (string, []byte) {
	t.Helper()
	configureCLI(t)
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	path := mustAssert[string](t, cfg.Targets[0].Config["path"])
	maildir, err := target.NewMaildirTarget(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	md := mustAssert[*target.MaildirTarget](t, maildir)
	md.SetName("local")
	if err := md.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })
	raw := []byte("Subject: fixture\r\n\r\nbody\r\n")
	locator, err := md.WriteBackupMessage(t.Context(), &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, filepath.FromSlash(locator)), []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(stateDir, "state.db")
	recordCorruptMaildirEntry(t, dbPath, md, locator, raw)
	return dbPath, readFileOrFatal(t, dbPath)
}

func recordCorruptMaildirEntry(t *testing.T, dbPath string, md *target.MaildirTarget, locator string, raw []byte) {
	t.Helper()
	store, err := state.NewStore(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBackupMessage(t.Context(), &plugins.BackupMessage{Source: "test-account", Target: plugins.TargetStateKey(md), Scope: "scope", Folder: "INBOX", UID: "1", Locator: locator, RawSHA256: dedup.ComputeContentHash(raw)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func readFileOrFatal(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func runVerifyCommand(t *testing.T) verificationReport {
	t.Helper()
	jsonOutput = true
	cmd := newVerifyCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetContext(t.Context())
	err := cmd.RunE(cmd, []string{"local"})
	if err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("verify error = %v", err)
	}
	var report verificationReport
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		t.Fatalf("decode report: %v; output=%s", decodeErr, output.String())
	}
	return report
}

func TestVerifyReportsCorruptMaildirWithoutChangingState(t *testing.T) {
	dbPath, before := setupCorruptMaildir(t)
	report := runVerifyCommand(t)
	if report.Corrupt != 1 {
		t.Fatalf("report = %+v", report)
	}
	after := readFileOrFatal(t, dbPath)
	if !bytes.Equal(before, after) {
		t.Fatal("read-only verification changed the state database")
	}
}

func TestVerifyDoesNotResolveMailboxCredentials(t *testing.T) {
	configureCLI(t)
	path := physicalTempDir(t)
	contents := fmt.Sprintf("sources:\n  - name: unavailable\n    type: imap\n    host: example.invalid\n    username: fixture\n    password_ref: env:MAILVAULT_TEST_ABSENT_PASSWORD\ntargets:\n  - name: local\n    type: maildir\n    config:\n      path: %q\npairs:\n  - source: unavailable\n    targets: [local]\n", path)
	t.Setenv("MAILVAULT_TEST_ABSENT_PASSWORD", "")
	if err := os.WriteFile(cfgFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newVerifyCommand()
	cmd.SetContext(t.Context())
	cmd.SetOut(&bytes.Buffer{})
	if err := cmd.RunE(cmd, []string{"local"}); err != nil {
		t.Fatalf("offline verify required source credentials: %v", err)
	}
}

type singleBackupReader struct {
	entry plugins.BackupMessage
	raw   []byte
}

func (r *singleBackupReader) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	return []plugins.BackupMessage{r.entry}, nil
}

func (r *singleBackupReader) ReadBackupMessage(context.Context, string) (*plugins.Message, error) {
	return &plugins.Message{Raw: r.raw}, nil
}

func TestVerifyDoesNotDoubleCountManifestedLegacyReceiptAfterCleanup(t *testing.T) {
	configureCLI(t)
	raw := []byte("Subject: fixture\n\nbody\n")
	hash := dedup.ComputeContentHash(raw)
	store, err := state.NewStore(t.Context(), filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSource(t.Context(), "src", "imap", "host", "user"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkMessageFetched(t.Context(), "src", "INBOX", "1", "<id>", hash, int64(len(raw)), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDelivered(t.Context(), "target", "hash:"+hash, "src", "INBOX", "1"); err != nil {
		t.Fatal(err)
	}
	entry := plugins.BackupMessage{Source: "src", Target: "target", Scope: "scope", Folder: "INBOX", UID: "1", Locator: "object-1", RawSHA256: hash}
	if err := store.RecordBackupMessage(t.Context(), &entry); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE messages SET fetched_at=datetime('now','-48 hours')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "DELETE FROM messages WHERE fetched_at < datetime('now','-24 hours')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := verifyTarget(t.Context(), "target", &singleBackupReader{entry: entry, raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified != 1 || report.Unverifiable != 0 {
		t.Fatalf("report = %+v; want one verified object and no duplicate legacy finding", report)
	}
}

type mboxRecordCase struct {
	name                string
	edit                func([]byte) []byte
	noFinalLF           bool
	empty               bool
	legacy              bool
	conflictingManifest bool
	want                verificationReport
}

func recordStructureCases() []mboxRecordCase {
	return []mboxRecordCase{
		{name: "unchanged", want: verificationReport{Scanned: 2, Verified: 2}},
		{name: "unchanged without final newline", noFinalLF: true, want: verificationReport{Scanned: 2, Verified: 2}},
		{name: "unchanged empty messages", empty: true, want: verificationReport{Scanned: 2, Verified: 2}},
		{name: "conflicting manifest extents", conflictingManifest: true, want: verificationReport{Scanned: 2, Verified: 1, Corrupt: 1}},
		{name: "separator no longer at line start", edit: neutralizeSeparator, want: verificationReport{Scanned: 1, Corrupt: 1, Missing: 1}},
		{name: "newline before separator removed", edit: joinSeparatorRecords, want: verificationReport{Scanned: 2, Verified: 0, Corrupt: 1, Missing: 1, Unverifiable: 1}},
		{name: "record extended", edit: extendRecord, want: verificationReport{Scanned: 2, Verified: 1, Corrupt: 1}},
		{name: "record extended without final newline", noFinalLF: true, edit: extendRecord, want: verificationReport{Scanned: 2, Verified: 1, Corrupt: 1}},
		{name: "split record", edit: splitRecord, want: verificationReport{Scanned: 3, Corrupt: 1, Missing: 1, Unverifiable: 2}},
		{name: "legacy receipt cannot verify changed extent", legacy: true, edit: mutateLegacyExtent, want: verificationReport{Scanned: 2, Corrupt: 1, Missing: 1, Verified: 1}},
	}
}

func neutralizeSeparator(data []byte) []byte {
	boundary := bytes.Index(data[1:], []byte("From MAILER-DAEMON ")) + 1
	data[boundary-1] = 'x'
	return data
}

func joinSeparatorRecords(data []byte) []byte {
	boundary := bytes.Index(data[1:], []byte("From MAILER-DAEMON ")) + 1
	return append(data[:boundary-1], data[boundary:]...)
}

func extendRecord(data []byte) []byte {
	return append(data, '\n')
}

func splitRecord(data []byte) []byte {
	return bytes.Replace(data, []byte("\n>>From escaped body"), []byte("\nFrom escaped body"), 1)
}

func mutateLegacyExtent(data []byte) []byte {
	return bytes.Replace(data, []byte("From MAILER-DAEMON "), []byte("From MAILER-DAEMON  "), 1)
}

func setupRecordStructureMbox(t *testing.T) (context.Context, string, *target.MboxTarget, *state.Store) {
	t.Helper()
	configureCLI(t)
	ctx := t.Context()
	path := filepath.Join(physicalTempDir(t), "archive.mbox")
	backup, err := target.NewMboxTarget(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	mbox := mustAssert[*target.MboxTarget](t, backup)
	if err := mbox.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mbox.Close() })
	store, err := state.NewStore(t.Context(), filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return ctx, path, mbox, store
}

func writeRecordFixture(ctx context.Context, t *testing.T, mbox *target.MboxTarget, store *state.Store, tc *mboxRecordCase, i int, raw []byte) {
	t.Helper()
	if tc.noFinalLF {
		raw = bytes.TrimSuffix(raw, []byte("\n"))
	}
	if tc.empty {
		raw = nil
	}
	locator, err := mbox.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	if tc.legacy {
		recordLegacyReceipt(ctx, t, store, i, raw)
	}
	if err := store.RecordBackupMessage(ctx, &plugins.BackupMessage{Source: "fixture", Target: "archive", Scope: "scope", Folder: "INBOX", UID: strconv.Itoa(i), Locator: locator, RawSHA256: dedup.ComputeContentHash(raw)}); err != nil {
		t.Fatal(err)
	}
	if tc.conflictingManifest && i == 0 {
		recordConflictingManifest(ctx, t, store, locator, raw)
	}
}

func recordLegacyReceipt(ctx context.Context, t *testing.T, store *state.Store, i int, raw []byte) {
	t.Helper()
	hash := dedup.ComputeContentHash(raw)
	if err := store.MarkMessageFetched(ctx, "fixture", "INBOX", strconv.Itoa(i+10), "", hash, int64(len(raw)), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDelivered(ctx, "archive", "hash:"+hash, "fixture", "INBOX", strconv.Itoa(i+10)); err != nil {
		t.Fatal(err)
	}
}

func recordConflictingManifest(ctx context.Context, t *testing.T, store *state.Store, locator string, raw []byte) {
	t.Helper()
	parts := strings.Split(locator, ":")
	parts[1] = "99999"
	if err := store.RecordBackupMessage(ctx, &plugins.BackupMessage{Source: "fixture", Target: "archive", Scope: "scope", Folder: "INBOX", UID: "alias", Locator: strings.Join(parts, ":"), RawSHA256: dedup.ComputeContentHash(raw)}); err != nil {
		t.Fatal(err)
	}
}

func applyRecordEdit(t *testing.T, path string, edit func([]byte) []byte) {
	t.Helper()
	if edit == nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edit(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertRecordReport(ctx context.Context, t *testing.T, mbox *target.MboxTarget, want verificationReport) {
	t.Helper()
	want.Target = "archive"
	report, err := verifyTarget(ctx, "archive", mbox)
	if err != nil || report != want {
		t.Fatalf("verify = %+v, %v; want %+v", report, err, want)
	}
}

func runRecordStructureCase(t *testing.T, tc *mboxRecordCase) {
	t.Helper()
	ctx, path, mbox, store := setupRecordStructureMbox(t)
	if tc.legacy {
		if err := store.UpsertSource(ctx, "fixture", "imap", "example.invalid", "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	for i, raw := range [][]byte{[]byte("Subject: first\n\n>From escaped body\n"), []byte("Subject: second\n\nsecond body\n")} { //nolint:dupword // fixture message content, not prose
		writeRecordFixture(ctx, t, mbox, store, tc, i, raw)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	applyRecordEdit(t, path, tc.edit)
	assertRecordReport(ctx, t, mbox, tc.want)
}

func TestVerifyMboxUsesCurrentRecordStructure(t *testing.T) {
	cases := recordStructureCases()
	for i := range cases {
		t.Run(cases[i].name, func(t *testing.T) {
			runRecordStructureCase(t, &cases[i])
		})
	}
}

type countingBackupReader struct {
	entries []plugins.BackupMessage
	raw     []byte
	reads   int
}

func (r *countingBackupReader) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	return r.entries, nil
}

func (r *countingBackupReader) ReadBackupMessage(context.Context, string) (*plugins.Message, error) {
	r.reads++
	return &plugins.Message{Raw: r.raw}, nil
}

func TestVerifyRequiresUniqueCurrentPhysicalEntry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		want  verificationReport
	}{
		{name: "missing", want: verificationReport{Missing: 1}},
		{name: "present", count: 1, want: verificationReport{Scanned: 1, Verified: 1}},
		{name: "duplicate", count: 2, want: verificationReport{Scanned: 2, Corrupt: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configureCLI(t)
			raw := []byte("Subject: fixture\n\nbody\n")
			entry := plugins.BackupMessage{Source: "fixture", Target: "archive", Scope: "scope", Folder: "INBOX", UID: "1", Locator: "object", RawSHA256: dedup.ComputeContentHash(raw)}
			store, err := state.NewStore(t.Context(), filepath.Join(stateDir, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordBackupMessage(t.Context(), &entry); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reader := &countingBackupReader{raw: raw}
			for range tc.count {
				reader.entries = append(reader.entries, entry)
			}
			report, err := verifyTarget(t.Context(), "archive", reader)
			tc.want.Target = "archive"
			if err != nil || report != tc.want {
				t.Fatalf("verify = %+v, %v; want %+v", report, err, tc.want)
			}
			wantReads := min(tc.count, 1)
			if reader.reads != wantReads {
				t.Fatalf("read count = %d, want %d current-entry reads", reader.reads, wantReads)
			}
		})
	}
}

type disappearingBackupReader struct {
	plugins.TargetBackupReader

	path string
}

func (r disappearingBackupReader) ReadBackupMessage(ctx context.Context, locator string) (*plugins.Message, error) {
	if err := os.Remove(filepath.Join(r.path, filepath.FromSlash(locator))); err != nil {
		return nil, err
	}
	return r.TargetBackupReader.ReadBackupMessage(ctx, locator)
}

func TestVerifyReportsMessageDisappearingAfterEnumeration(t *testing.T) {
	for _, database := range []string{"absent", "empty", "manifested"} {
		t.Run(database, func(t *testing.T) {
			oldState := stateDir
			stateDir = physicalTempDir(t)
			t.Cleanup(func() { stateDir = oldState })
			ctx := t.Context()
			path := physicalTempDir(t)
			backup, err := target.NewMaildirTarget(map[string]any{"path": path})
			if err != nil {
				t.Fatal(err)
			}
			defer backup.Close()
			if err := backup.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			md := mustAssert[*target.MaildirTarget](t, backup)
			raw := []byte("Subject: synthetic\n\nfixture\n")
			locator, err := md.WriteBackupMessage(ctx, &plugins.Message{Raw: raw})
			if err != nil {
				t.Fatal(err)
			}
			if database != "absent" {
				store, err := state.NewStore(t.Context(), filepath.Join(stateDir, "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				if database == "manifested" {
					err = store.RecordBackupMessage(ctx, &plugins.BackupMessage{Source: "fixture", Target: "archive", Scope: "scope", Folder: "INBOX", UID: "1", Locator: locator, RawSHA256: dedup.ComputeContentHash(raw)})
				}
				closeErr := store.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("prepare manifest: %v, close: %v", err, closeErr)
				}
			}
			report, err := verifyTarget(ctx, "archive", disappearingBackupReader{TargetBackupReader: md, path: path})
			want := verificationReport{Target: "archive", Scanned: 1, Missing: 1}
			if err != nil || report != want {
				t.Fatalf("verify = %+v, %v; want %+v", report, err, want)
			}
		})
	}
}
