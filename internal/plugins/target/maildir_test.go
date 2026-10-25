package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMaildirUniqueNamesWithFrozenClock(t *testing.T) {
	path := t.TempDir()
	target, err := NewMaildirTarget(map[string]interface{}{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		for i := 0; i < 10; i++ {
			if err := target.WriteMessage(context.Background(), &plugins.Message{Raw: []byte(fmt.Sprintf("message-%d", i))}); err != nil {
				t.Fatal(err)
			}
		}
	})
	files, err := os.ReadDir(filepath.Join(path, "new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 10 {
		t.Fatalf("10 writes produced %d files", len(files))
	}
	seen := make(map[string]bool)
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(path, "new", file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		seen[string(data)] = true
	}
	if len(seen) != 10 {
		t.Fatal("message contents overwritten")
	}
}

func TestFilesystemTargetsHonorCreateIfMissing(t *testing.T) {
	for _, kind := range []string{"maildir", "mbox"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing", "mail")
			factory := NewMaildirTarget
			if kind == "mbox" {
				factory = NewMboxTarget
			}
			target, err := factory(map[string]interface{}{"path": path, "create_if_missing": false})
			if err != nil {
				t.Fatal(err)
			}
			if err := target.Initialize(context.Background()); err == nil {
				_ = target.Close()
				t.Fatal("missing path was created despite create_if_missing=false")
			}
			if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatalf("initialization changed filesystem: %v", err)
			}
			created, err := factory(map[string]interface{}{"path": path})
			if err != nil {
				t.Fatal(err)
			}
			if err := created.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := created.Close(); err != nil {
				t.Fatal(err)
			}
			if err := target.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := target.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMaildirConcurrentInstances(t *testing.T) {
	path := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		target, err := NewMaildirTarget(map[string]interface{}{"path": path})
		if err != nil {
			t.Fatal(err)
		}
		if err := target.Initialize(context.Background()); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if err := target.WriteMessage(context.Background(), &plugins.Message{Raw: []byte("test")}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	files, err := os.ReadDir(filepath.Join(path, "new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 400 {
		t.Fatalf("writes produced %d files, want 400", len(files))
	}
}

func TestMaildirCleanupFailureAfterPublicationDoesNotReturnError(t *testing.T) {
	path := t.TempDir()
	target, err := NewMaildirTarget(map[string]interface{}{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	maildir := target.(*MaildirTarget)
	if err := maildir.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	cleanupErr := errors.New("injected cleanup permission error")
	maildir.removeTemp = func(*maildirDirectory, string) error { return cleanupErr }
	message := []byte("complete message")
	if err := maildir.WriteMessage(context.Background(), &plugins.Message{Raw: message}); err != nil {
		t.Fatalf("published message returned cleanup error: %v", err)
	}
	files, err := os.ReadDir(filepath.Join(path, "new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("published %d messages, want exactly one", len(files))
	}
	got, err := os.ReadFile(filepath.Join(path, "new", files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(message) {
		t.Fatalf("published %q, want %q", got, message)
	}
}

func TestMaildirDefaultFilesystemHooks(t *testing.T) {
	target := &MaildirTarget{path: t.TempDir(), create: true}
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := target.WriteMessage(context.Background(), &plugins.Message{Raw: []byte("test")}); err != nil {
		t.Fatal(err)
	}
}

func TestMaildirPrePublicationErrorIncludesCleanupFailure(t *testing.T) {
	path := t.TempDir()
	target, err := NewMaildirTarget(map[string]interface{}{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	maildir := target.(*MaildirTarget)
	if err := maildir.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	primaryErr := errors.New("publication blocked")
	cleanupErr := errors.New("injected cleanup permission error")
	maildir.link = func(*maildirDirectory, string, *maildirDirectory, string) error { return primaryErr }
	maildir.removeTemp = func(*maildirDirectory, string) error { return cleanupErr }
	err = maildir.WriteMessage(context.Background(), &plugins.Message{Raw: []byte("message")})
	if !errors.Is(err, primaryErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("error = %v, want primary and cleanup errors", err)
	}
}
