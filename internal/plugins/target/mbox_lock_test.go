package target

import (
	"context"
	"errors"
	"fmt"
	"github.com/zekihan/mailvault/internal/plugins"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMboxLockHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox")
	holder := &MboxTarget{path: path, create: true}
	second := &MboxTarget{path: path}
	ctx := t.Context()
	if err := holder.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := second.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := lockMbox(ctx, holder.file); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	err := second.WriteMessage(canceled, &plugins.Message{Raw: []byte("Subject: test\n\nbody\n")})
	cancel()
	unlockErr := unlockMbox(holder.file)
	if unlockErr != nil {
		t.Fatal(unlockErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked write ignored deadline: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatal("canceled append modified mailbox")
	}
	if err := second.WriteMessage(ctx, &plugins.Message{Raw: []byte("Subject: test\n\nbody\n")}); err != nil {
		t.Fatal(err)
	}
}

// Cancel immediately after the lock helper samples Err, reproducing cancellation
// between its initial check and successful acquisition without timing sleeps.
type cancelDuringLock struct {
	context.Context //nolint:containedctx // test-only ctx wrapper; must wrap the parent ctx

	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelDuringLock) Err() error {
	err := c.Context.Err()
	c.once.Do(c.cancel)
	return err
}

func TestMboxRechecksCancellationAfterLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox")
	target := &MboxTarget{path: path, create: true}
	if err := target.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := target.WriteMessage(&cancelDuringLock{Context: ctx, cancel: cancel}, &plugins.Message{Raw: []byte("Subject: test\n\nbody\n")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("write ignored cancellation after lock acquisition: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Fatalf("canceled append changed mailbox: %v", err)
	}
}

func TestMboxSerializesIndependentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox")
	var wg sync.WaitGroup
	for i := range 4 {
		target := &MboxTarget{path: path, create: true}
		if err := target.Initialize(t.Context()); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer target.Close()
			for j := range 25 {
				raw := []byte(fmt.Sprintf("Subject: test\n\nbody-%d-%d\n", i, j))
				if err := target.WriteMessage(t.Context(), &plugins.Message{Raw: raw}); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "From MAILER-DAEMON ") != 100 {
		t.Fatal("lost or damaged records")
	}
	for i := range 4 {
		for j := range 25 {
			if strings.Count(string(data), fmt.Sprintf("\nbody-%d-%d\n", i, j)) != 1 {
				t.Fatal("interleaved or missing body")
			}
		}
	}
}
