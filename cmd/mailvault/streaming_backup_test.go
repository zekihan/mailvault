package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

func (verifier *streamingVerificationReader) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	verifier.t.Fatal("consumer materialized the physical listing")
	return nil, nil
}
func (verifier *streamingVerificationReader) WalkBackupMessages(_ context.Context, visit func(plugins.BackupMessage) error) error {
	for i := range 2 {
		if verifier.reads != i {
			verifier.t.Fatalf("advanced listing before reading entry %d", i)
		}
		if err := visit(plugins.BackupMessage{Locator: strconv.Itoa(i)}); err != nil {
			return err
		}
	}
	return verifier.failure
}
func (verifier *streamingVerificationReader) ReadBackupMessage(context.Context, string) (*plugins.Message, error) {
	verifier.reads++
	return &plugins.Message{Raw: []byte("Subject: fixture\n\nbody\n")}, nil
}
func TestVerificationConsumesEntriesBeforeAdvancingListing(t *testing.T) {
	previous := stateDir
	stateDir = physicalTempDir(t)
	t.Cleanup(func() { stateDir = previous })
	for _, failure := range []error{nil, plugins.ErrBackupListingLimit} {
		reader := &streamingVerificationReader{t: t, failure: failure}
		report, err := verifyTarget(t.Context(), "fixture", reader)
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
func setupPinnedMaildir(ctx context.Context, t *testing.T) (string, plugins.Target) {
	t.Helper()
	path := filepath.Join(physicalTempDir(t), "maildir")
	backup, err := target.NewMaildirTarget(map[string]any{"path": path, "max_listing_entries": 64, "max_listing_locator_bytes": 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backup.Close() })
	if err := backup.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := backup.WriteMessage(ctx, &plugins.Message{Raw: []byte("Subject: fixture\n\nbody\n")}); err != nil {
			t.Fatal(err)
		}
	}
	return path, backup
}

func pinnedRestoreMutation(mode, path string, cancel context.CancelFunc) func() error {
	return func() error {
		if mode == "cancellation" {
			cancel()
			return nil
		}
		directory := filepath.Join(path, "new")
		if err := os.Rename(directory, directory+"-old"); err != nil {
			return err
		}
		return os.Mkdir(directory, 0o700)
	}
}

func runPinnedRestore(ctx context.Context, t *testing.T, mode, path string, backup plugins.Target) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader := mutatingMaildirRestoreReader{TargetBackupReader: mustAssert[plugins.TargetBackupReader](t, backup), walker: mustAssert[plugins.TargetBackupWalker](t, backup)}
	reader.after = pinnedRestoreMutation(mode, path, cancel)
	store, err := state.NewStore(ctx, filepath.Join(physicalTempDir(t), "state.db"))
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
		if err == nil || (mode == "cancellation" && !errors.Is(err, context.Canceled)) {
			t.Fatalf("preflight error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restore preflight deadlocked in Maildir callback")
	}
	if appender.created || len(appender.appended) != 0 {
		t.Fatalf("failed preflight touched mailbox: %+v", appender)
	}
}

func TestPinnedMaildirRestoreAbortsBeforeMailboxCreation(t *testing.T) {
	for _, mode := range []string{"cancellation", "directory replacement"} {
		t.Run(mode, func(t *testing.T) {
			path, backup := setupPinnedMaildir(t.Context(), t)
			runPinnedRestore(t.Context(), t, mode, path, backup)
		})
	}
}
