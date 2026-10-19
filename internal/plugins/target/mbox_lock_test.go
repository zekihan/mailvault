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
	a := &MboxTarget{path: path, create: true}
	b := &MboxTarget{path: path}
	ctx := context.Background()
	if err := a.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := b.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := lockMbox(ctx, a.file); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	err := b.WriteMessage(canceled, &plugins.Message{Raw: []byte("Subject: test\n\nbody\n")})
	cancel()
	unlockErr := unlockMbox(a.file)
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
	if err := b.WriteMessage(ctx, &plugins.Message{Raw: []byte("Subject: test\n\nbody\n")}); err != nil {
		t.Fatal(err)
	}
}

// Cancel immediately after the lock helper samples Err, reproducing cancellation
// between its initial check and successful acquisition without timing sleeps.
type cancelDuringLock struct {
	context.Context
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
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	ctx, cancel := context.WithCancel(context.Background())
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
	for i := 0; i < 4; i++ {
		target := &MboxTarget{path: path, create: true}
		if err := target.Initialize(context.Background()); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer target.Close()
			for j := 0; j < 25; j++ {
				raw := []byte(fmt.Sprintf("Subject: test\n\nbody-%d-%d\n", i, j))
				if err := target.WriteMessage(context.Background(), &plugins.Message{Raw: raw}); err != nil {
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
	for i := 0; i < 4; i++ {
		for j := 0; j < 25; j++ {
			if strings.Count(string(data), fmt.Sprintf("\nbody-%d-%d\n", i, j)) != 1 {
				t.Fatal("interleaved or missing body")
			}
		}
	}
}
