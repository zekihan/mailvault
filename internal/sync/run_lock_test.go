package sync

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/state"
)

type runLockSource struct {
	fixtureSource

	fetches *atomic.Int32
}

func (source *runLockSource) FetchMessages(ctx context.Context, folders []string, since, until int64) (<-chan *plugins.Message, <-chan error) {
	source.fetches.Add(1)
	return source.fixtureSource.FetchMessages(ctx, folders, since, until)
}

type runLockTarget struct {
	*target.MboxTarget

	writes  *atomic.Int32
	entered chan struct{}
	release <-chan struct{}
}

func (runTarget *runLockTarget) WriteMessage(ctx context.Context, message *plugins.Message) error {
	if runTarget.writes.Add(1) == 1 {
		close(runTarget.entered)
		select {
		case <-runTarget.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return runTarget.MboxTarget.WriteMessage(ctx, message)
}

type runLockEngines struct {
	engines []*SyncEngine
	entered chan struct{}
	release chan struct{}
	fetches atomic.Int32
	writes  atomic.Int32
}

func newRunLockEngines(t *testing.T, root string) *runLockEngines {
	t.Helper()
	database := filepath.Join(root, "state.db")
	mailbox := filepath.Join(root, "archive.mbox")
	locks := &runLockEngines{entered: make(chan struct{}), release: make(chan struct{})}
	raw := []byte("Message-ID: <fixture@example.invalid>\r\n\r\nfixture body\r\n")
	cfg := &config.Config{Sources: []config.SourceConfig{{Name: "source", Type: "fixture", Folders: []string{"INBOX"}}}, Targets: []config.TargetConfig{{Name: "archive", Type: "mbox"}}, Pairs: []config.PairConfig{{Source: "source", Targets: []string{"archive"}}}}
	for range 2 {
		locks.engines = append(locks.engines, newRunLockEngine(t, locks, cfg, database, mailbox, raw))
	}
	return locks
}

func newRunLockEngine(t *testing.T, locks *runLockEngines, cfg *config.Config, database, mailbox string, raw []byte) *SyncEngine {
	t.Helper()
	store, err := state.NewStore(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]any) (plugins.Source, error) {
		return &runLockSource{fixtureSource{name: "source", messages: []*plugins.Message{{UID: "1", Folder: "INBOX", Raw: raw}}}, &locks.fetches}, nil
	})
	registry.RegisterTarget("mbox", func(map[string]any) (plugins.Target, error) {
		p, err := target.NewMboxTarget(map[string]any{"path": mailbox, "create_if_missing": true})
		if err != nil {
			return nil, err
		}
		return &runLockTarget{MboxTarget: mustAssert[*target.MboxTarget](t, p), writes: &locks.writes, entered: locks.entered, release: locks.release}, nil
	})
	engine, err := NewSyncEngine(cfg, store, registry, WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

type lockRunOutcome struct {
	stats *SyncStats
	err   error
}

func assertWaiterIdle(t *testing.T, locks *runLockEngines) {
	t.Helper()
	if locks.fetches.Load() != 1 || locks.writes.Load() != 1 {
		t.Fatalf("waiter performed work: fetches=%d writes=%d", locks.fetches.Load(), locks.writes.Load())
	}
}

func assertRunLockResult(t *testing.T, locks *runLockEngines, mailbox string, raw []byte, first lockRunOutcome) {
	t.Helper()
	if first.err != nil {
		t.Fatal(first.err)
	}
	assertStats(t, first.stats, 1, 0, 0)
	second, err := locks.engines[1].Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assertStats(t, second, 0, 1, 0)
	content, err := os.ReadFile(mailbox)
	if err != nil {
		t.Fatal(err)
	}
	if copies := bytes.Count(content, raw); copies != 1 {
		t.Fatalf("overlapping runs left %d copies", copies)
	}
}

func TestOverlappingRunWaitsWithoutFetchingAndCanCancel(t *testing.T) {
	root := physicalTempDir(t)
	mailbox := filepath.Join(root, "archive.mbox")
	raw := []byte("Message-ID: <fixture@example.invalid>\r\n\r\nfixture body\r\n")
	locks := newRunLockEngines(t, root)
	firstDone := make(chan lockRunOutcome, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	go func() { stats, err := locks.engines[0].Run(ctx); firstDone <- lockRunOutcome{stats, err} }()
	select {
	case <-locks.entered:
	case <-ctx.Done():
		t.Fatal("first run did not reach its write")
	}
	finished := false
	defer func() {
		if !finished {
			close(locks.release)
			<-firstDone
		}
	}()
	waitingCtx, stop := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer stop()
	_, err := locks.engines[1].Run(waitingCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("overlapping run did not wait for ownership: %v", err)
	}
	assertWaiterIdle(t, locks)
	close(locks.release)
	first := <-firstDone
	finished = true
	assertRunLockResult(t, locks, mailbox, raw, first)
}
