package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/restore"
	"github.com/zekihan/mailvault/internal/state"
)

type streamingVerificationReader struct {
	t       *testing.T
	reads   int
	failure error
}

func (r *streamingVerificationReader) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	r.t.Fatal("consumer materialized the physical listing")
	return nil, nil
}
func (r *streamingVerificationReader) WalkBackupMessages(_ context.Context, visit func(plugins.BackupMessage) error) error {
	for i := 0; i < 2; i++ {
		if r.reads != i {
			r.t.Fatalf("advanced listing before reading entry %d", i)
		}
		if err := visit(plugins.BackupMessage{Locator: fmt.Sprint(i)}); err != nil {
			return err
		}
	}
	return r.failure
}
func (r *streamingVerificationReader) ReadBackupMessage(context.Context, string) (*plugins.Message, error) {
	r.reads++
	return &plugins.Message{Raw: []byte("Subject: fixture\n\nbody\n")}, nil
}
func TestVerificationConsumesEntriesBeforeAdvancingListing(t *testing.T) {
	previous := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = previous })
	for _, failure := range []error{nil, plugins.ErrBackupListingLimit} {
		reader := &streamingVerificationReader{t: t, failure: failure}
		report, err := verifyTarget(context.Background(), "fixture", reader)
		if !errors.Is(err, failure) || report.Scanned != 2 {
			t.Fatalf("report=%+v, error=%v", report, err)
		}
		if failure == nil && report.Unverifiable != 2 {
			t.Fatalf("report=%+v", report)
		}
	}
}

// Apply a fixture mutation after restore reads its first entry, while still
// inside the real pinned-directory walk.
type mutatingMaildirRestoreReader struct {
	plugins.TargetBackupReader
	walker plugins.TargetBackupWalker
	after  func() error
}

func (r mutatingMaildirRestoreReader) WalkBackupMessages(ctx context.Context, visit func(plugins.BackupMessage) error) error {
	return r.walker.WalkBackupMessages(ctx, func(entry plugins.BackupMessage) error {
		if err := visit(entry); err != nil {
			return err
		}
		return r.after()
	})
}
func TestPinnedMaildirRestoreAbortsBeforeMailboxCreation(t *testing.T) {
	for _, mode := range []string{"cancellation", "directory replacement"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "maildir")
			backup, err := target.NewMaildirTarget(map[string]any{"path": path, "max_listing_entries": 64, "max_listing_locator_bytes": 4096})
			if err != nil {
				t.Fatal(err)
			}
			if err := backup.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := backup.WriteMessage(context.Background(), &plugins.Message{Raw: []byte("Subject: fixture\n\nbody\n")}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader := mutatingMaildirRestoreReader{TargetBackupReader: backup.(plugins.TargetBackupReader), walker: backup.(plugins.TargetBackupWalker)}
			reader.after = func() error {
				if mode == "cancellation" {
					cancel()
					return nil
				}
				directory := filepath.Join(path, "new")
				if err := os.Rename(directory, directory+"-old"); err != nil {
					return err
				}
				return os.Mkdir(directory, 0700)
			}
			store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			appender := &recordingMailbox{}
			done := make(chan error, 1)
			go func() {
				_, err := restore.Run(ctx, reader, appender, store, restore.Options{BackupTarget: "archive", DestinationSource: "fixture", DestinationFolder: "Restored", AllowUnverified: true})
				done <- err
			}()
			select {
			case err := <-done:
				defer backup.Close()
				if err == nil || (mode == "cancellation" && !errors.Is(err, context.Canceled)) {
					t.Fatalf("preflight error=%v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("restore preflight deadlocked in Maildir callback")
			}
			if appender.created || len(appender.appended) != 0 {
				t.Fatalf("failed preflight touched mailbox: %+v", appender)
			}
		})
	}
}
