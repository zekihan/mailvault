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
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
)

func streamingMaildirFixture(t *testing.T) *MaildirTarget {
	t.Helper()
	value, err := NewMaildirTarget(map[string]any{"path": filepath.Join(t.TempDir(), "maildir"), "max_listing_entries": 64, "max_listing_locator_bytes": 4096})
	if err != nil {
		t.Fatal(err)
	}
	target := value.(*MaildirTarget)
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return target
}

// A watchdog makes a callback mutex regression fail promptly. Avoid attempting
// Close on a wedged target, since that would hide the failure behind a timeout.
func runMaildirWalk(t *testing.T, target *MaildirTarget, ctx context.Context, visit func(plugins.BackupMessage) error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- target.WalkBackupMessages(ctx, visit) }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Maildir walker callback deadlocked")
		return nil
	}
}

func TestPinnedMaildirWalkCallbacksReadResolveAndRepair(t *testing.T) {
	target := streamingMaildirFixture(t)
	raw := []byte("Subject: callback fixture\n\nbody\n")
	locator, err := target.WriteBackupMessage(context.Background(), &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("one callback completed")
	var repaired string
	err = runMaildirWalk(t, target, context.Background(), func(entry plugins.BackupMessage) error {
		message, err := target.ReadBackupMessage(context.Background(), entry.Locator)
		if err != nil {
			return err
		}
		if !bytes.Equal(message.Raw, raw) {
			return errors.New("wrong callback message")
		}
		if _, err := target.ResolveBackupLocator(locator); err != nil {
			return err
		}
		repaired, err = target.WriteRepairMessage(context.Background(), message)
		if err != nil {
			return err
		}
		return stop
	})
	// Register cleanup only after proving callback re-entry did not wedge the lock.
	defer target.Close()
	if !errors.Is(err, stop) || repaired == "" || repaired == locator {
		t.Fatalf("callback repair=%q, error=%v", repaired, err)
	}
	message, err := target.ReadBackupMessage(context.Background(), repaired)
	if err != nil || !bytes.Equal(message.Raw, raw) {
		t.Fatalf("repaired message=%+v, error=%v", message, err)
	}
}

func TestPinnedMaildirWalkCancellationAfterRead(t *testing.T) {
	target := streamingMaildirFixture(t)
	for range 4 {
		if err := target.WriteMessage(context.Background(), &plugins.Message{Raw: []byte("fixture")}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := runMaildirWalk(t, target, ctx, func(entry plugins.BackupMessage) error {
		if _, err := target.ReadBackupMessage(ctx, entry.Locator); err != nil {
			return err
		}
		calls++
		cancel()
		return nil
	})
	defer target.Close()
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d, error=%v", calls, err)
	}
	// The canceled walk closed only its cloned handles, leaving the target usable.
	if err := target.WriteMessage(context.Background(), &plugins.Message{Raw: []byte("after cancellation")}); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedMaildirWalkRejectsDirectoryReplacement(t *testing.T) {
	for _, component := range []string{"root", "new"} {
		t.Run(component, func(t *testing.T) {
			target := streamingMaildirFixture(t)
			for range 2 {
				if err := target.WriteMessage(context.Background(), &plugins.Message{Raw: []byte("safe fixture")}); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			err := runMaildirWalk(t, target, context.Background(), func(entry plugins.BackupMessage) error {
				message, err := target.ReadBackupMessage(context.Background(), entry.Locator)
				if err != nil {
					return err
				}
				if string(message.Raw) != "safe fixture" {
					return errors.New("read replacement directory")
				}
				calls++
				path := target.path
				if component != "root" {
					path = filepath.Join(path, component)
				}
				if err := os.Rename(path, path+"-old"); err != nil {
					if runtime.GOOS == "windows" {
						t.Logf("Windows denied replacement of a pinned directory: %v", err)
						return err
					}
					return fmt.Errorf("replace fixture directory: %w", err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					return err
				}
				if component == "root" {
					path = filepath.Join(path, "new")
					if err := os.Mkdir(path, 0700); err != nil {
						return err
					}
				}
				return os.WriteFile(filepath.Join(path, "replacement"), []byte("foreign fixture"), 0600)
			})
			defer target.Close()
			if err == nil || calls != 1 {
				t.Fatalf("accepted replacement: calls=%d, error=%v", calls, err)
			}
		})
	}
}

func TestPinnedMaildirRenamedLookupBudgets(t *testing.T) {
	target := streamingMaildirFixture(t)
	defer target.Close()
	raw := []byte("safe fixture")
	locator, err := target.WriteBackupMessage(context.Background(), &plugins.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(target.path, "cur", filepath.Base(locator))
	if err := os.Rename(filepath.Join(target.path, filepath.FromSlash(locator)), moved); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(target.path, "cur", fmt.Sprint("other-", i)), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, limits := range []backupListingLimits{{entries: 1, locatorBytes: 4096}, {entries: 100, locatorBytes: 1}} {
		target.listingLimits = limits
		if _, err := target.ReadBackupMessage(context.Background(), locator); !errors.Is(err, plugins.ErrBackupListingLimit) {
			t.Fatalf("renamed read exceeded scan budget: %v", err)
		}
		if _, err := target.ResolveBackupLocator(locator); !errors.Is(err, plugins.ErrBackupListingLimit) {
			t.Fatalf("renamed resolution exceeded scan budget: %v", err)
		}
	}
	target.listingLimits = backupListingLimits{entries: 100, locatorBytes: 4096}
	got, err := target.ReadBackupMessage(context.Background(), locator)
	if err != nil || !bytes.Equal(got.Raw, raw) {
		t.Fatalf("renamed read=%+v, %v", got, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := target.ReadBackupMessage(canceled, locator); !errors.Is(err, context.Canceled) {
		t.Fatalf("renamed read ignored cancellation: %v", err)
	}
}

func TestPinnedMaildirDirectoryBatchStopsOnCancellation(t *testing.T) {
	target := streamingMaildirFixture(t)
	defer target.Close()
	directory, err := target.root.child("new", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	for i := 0; i < backupDirectoryBatchSize+2; i++ {
		if err := directory.root.WriteFile(fmt.Sprint(i), []byte(strings.Repeat("x", 8)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err = directory.walkEntries(ctx, (backupListingLimits{}).budget(), "new", func(os.DirEntry) error { calls++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("directory batch ignored cancellation: calls=%d, error=%v", calls, err)
	}
}
