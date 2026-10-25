package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/state"
)

func TestVerifyReportsCorruptMaildirWithoutChangingState(t *testing.T) {
	configureCLI(t)
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Targets[0].Config["path"].(string)
	maildir, err := target.NewMaildirTarget(map[string]interface{}{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	md := maildir.(*target.MaildirTarget)
	md.SetName("local")
	if err := md.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: fixture\r\n\r\nbody\r\n")
	locator, err := md.WriteBackupMessage(context.Background(), &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, filepath.FromSlash(locator)), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(stateDir, "state.db")
	store, err := state.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBackupMessage(context.Background(), plugins.BackupMessage{Source: "test-account", Target: plugins.TargetStateKey(md), Scope: "scope", Folder: "INBOX", UID: "1", Locator: locator, RawSHA256: dedup.ComputeContentHash(raw)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	jsonOutput = true
	cmd := newVerifyCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetContext(context.Background())
	err = cmd.RunE(cmd, []string{"local"})
	if err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("verify error = %v", err)
	}
	var report verificationReport
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		t.Fatalf("decode report: %v; output=%s", decodeErr, output.String())
	}
	if report.Corrupt != 1 {
		t.Fatalf("report = %+v", report)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only verification changed the state database")
	}
}

func TestVerifyDoesNotResolveMailboxCredentials(t *testing.T) {
	configureCLI(t)
	path := t.TempDir()
	contents := fmt.Sprintf("sources:\n  - name: unavailable\n    type: imap\n    host: example.invalid\n    username: fixture\n    password_ref: env:MAILVAULT_TEST_ABSENT_PASSWORD\ntargets:\n  - name: local\n    type: maildir\n    config:\n      path: %q\npairs:\n  - source: unavailable\n    targets: [local]\n", path)
	t.Setenv("MAILVAULT_TEST_ABSENT_PASSWORD", "")
	if err := os.WriteFile(cfgFile, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newVerifyCommand()
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	if err := cmd.RunE(cmd, []string{"local"}); err != nil {
		t.Fatalf("offline verify required source credentials: %v", err)
	}
}

type singleBackupReader struct {
	entry plugins.BackupMessage
	raw   []byte
}

func (r singleBackupReader) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	return []plugins.BackupMessage{r.entry}, nil
}

func (r singleBackupReader) ReadBackupMessage(context.Context, string) (*plugins.Message, error) {
	return &plugins.Message{Raw: r.raw}, nil
}

func TestVerifyDoesNotDoubleCountManifestedLegacyReceiptAfterCleanup(t *testing.T) {
	configureCLI(t)
	raw := []byte("Subject: fixture\n\nbody\n")
	hash := dedup.ComputeContentHash(raw)
	store, err := state.NewStore(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSource(context.Background(), "src", "imap", "host", "user"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkMessageFetched(context.Background(), "src", "INBOX", "1", "<id>", hash, int64(len(raw)), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDelivered(context.Background(), "target", "hash:"+hash, "src", "INBOX", "1"); err != nil {
		t.Fatal(err)
	}
	entry := plugins.BackupMessage{Source: "src", Target: "target", Scope: "scope", Folder: "INBOX", UID: "1", Locator: "object-1", RawSHA256: hash}
	if err := store.RecordBackupMessage(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE messages SET fetched_at=datetime('now','-48 hours')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM messages WHERE fetched_at < datetime('now','-24 hours')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := verifyTarget(context.Background(), "target", singleBackupReader{entry: entry, raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified != 1 || report.Unverifiable != 0 {
		t.Fatalf("report = %+v; want one verified object and no duplicate legacy finding", report)
	}
}

func TestVerifyMboxUsesCurrentRecordStructure(t *testing.T) {
	for _, tc := range []struct {
		name                string
		edit                func([]byte) []byte
		noFinalLF           bool
		empty               bool
		legacy              bool
		conflictingManifest bool
		want                verificationReport
	}{
		{name: "unchanged", want: verificationReport{Scanned: 2, Verified: 2}},
		{name: "unchanged without final newline", noFinalLF: true, want: verificationReport{Scanned: 2, Verified: 2}},
		{name: "unchanged empty messages", empty: true, want: verificationReport{Scanned: 2, Verified: 2}},
		{name: "conflicting manifest extents", conflictingManifest: true, want: verificationReport{Scanned: 2, Verified: 1, Corrupt: 1}},
		{name: "separator no longer at line start", edit: func(data []byte) []byte {
			boundary := bytes.Index(data[1:], []byte("From MAILER-DAEMON ")) + 1
			data[boundary-1] = 'x' // Preserve both historical offsets and raw ranges.
			return data
		}, want: verificationReport{Scanned: 1, Corrupt: 1, Missing: 1}},
		{name: "newline before separator removed", edit: func(data []byte) []byte {
			boundary := bytes.Index(data[1:], []byte("From MAILER-DAEMON ")) + 1
			return append(data[:boundary-1], data[boundary:]...)
		}, want: verificationReport{Scanned: 2, Verified: 0, Corrupt: 1, Missing: 1, Unverifiable: 1}},
		{name: "record extended", edit: func(data []byte) []byte {
			return append(data, '\n')
		}, want: verificationReport{Scanned: 2, Verified: 1, Corrupt: 1}},
		{name: "record extended without final newline", noFinalLF: true, edit: func(data []byte) []byte {
			return append(data, '\n')
		}, want: verificationReport{Scanned: 2, Verified: 1, Corrupt: 1}},
		{name: "split record", edit: func(data []byte) []byte {
			return bytes.Replace(data, []byte("\n>>From escaped body"), []byte("\nFrom escaped body"), 1)
		}, want: verificationReport{Scanned: 3, Corrupt: 1, Missing: 1, Unverifiable: 2}},
		{name: "legacy receipt cannot verify changed extent", legacy: true, edit: func(data []byte) []byte {
			return bytes.Replace(data, []byte("From MAILER-DAEMON "), []byte("From MAILER-DAEMON  "), 1)
		}, want: verificationReport{Scanned: 2, Corrupt: 1, Missing: 1, Verified: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configureCLI(t)
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "archive.mbox")
			backup, err := target.NewMboxTarget(map[string]interface{}{"path": path})
			if err != nil {
				t.Fatal(err)
			}
			mbox := backup.(*target.MboxTarget)
			if err := mbox.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = mbox.Close() })
			store, err := state.NewStore(filepath.Join(stateDir, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.legacy {
				if err := store.UpsertSource(ctx, "fixture", "imap", "example.invalid", "fixture"); err != nil {
					t.Fatal(err)
				}
			}
			for i, raw := range [][]byte{[]byte("Subject: first\n\n>From escaped body\n"), []byte("Subject: second\n\nsecond body\n")} {
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
					hash := dedup.ComputeContentHash(raw)
					if err := store.MarkMessageFetched(ctx, "fixture", "INBOX", fmt.Sprint(i+10), "", hash, int64(len(raw)), ""); err != nil {
						t.Fatal(err)
					}
					if err := store.MarkDelivered(ctx, "archive", "hash:"+hash, "fixture", "INBOX", fmt.Sprint(i+10)); err != nil {
						t.Fatal(err)
					}
				}
				if err := store.RecordBackupMessage(ctx, plugins.BackupMessage{Source: "fixture", Target: "archive", Scope: "scope", Folder: "INBOX", UID: fmt.Sprint(i), Locator: locator, RawSHA256: dedup.ComputeContentHash(raw)}); err != nil {
					t.Fatal(err)
				}
				if tc.conflictingManifest && i == 0 {
					parts := strings.Split(locator, ":")
					parts[1] = "99999"
					if err := store.RecordBackupMessage(ctx, plugins.BackupMessage{Source: "fixture", Target: "archive", Scope: "scope", Folder: "INBOX", UID: "alias", Locator: strings.Join(parts, ":"), RawSHA256: dedup.ComputeContentHash(raw)}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if tc.edit != nil {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, tc.edit(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			report, err := verifyTarget(ctx, "archive", mbox)
			tc.want.Target = "archive"
			if err != nil || report != tc.want {
				t.Fatalf("verify = %+v, %v; want %+v", report, err, tc.want)
			}
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
			store, err := state.NewStore(filepath.Join(stateDir, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordBackupMessage(context.Background(), entry); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reader := &countingBackupReader{raw: raw}
			for i := 0; i < tc.count; i++ {
				reader.entries = append(reader.entries, entry)
			}
			report, err := verifyTarget(context.Background(), "archive", reader)
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
