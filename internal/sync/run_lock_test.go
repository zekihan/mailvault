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

func (s *runLockSource) FetchMessages(ctx context.Context, folders []string, since, until int64) (<-chan *plugins.Message, <-chan error) {
	s.fetches.Add(1)
	return s.fixtureSource.FetchMessages(ctx, folders, since, until)
}

type runLockTarget struct {
	*target.MboxTarget
	writes  *atomic.Int32
	entered chan struct{}
	release <-chan struct{}
}

func (t *runLockTarget) WriteMessage(ctx context.Context, message *plugins.Message) error {
	if t.writes.Add(1) == 1 {
		close(t.entered)
		select {
		case <-t.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return t.MboxTarget.WriteMessage(ctx, message)
}

func TestOverlappingRunWaitsWithoutFetchingAndCanCancel(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(root, "state.db")
	mailbox := filepath.Join(root, "archive.mbox")
	entered, release := make(chan struct{}), make(chan struct{})
	var fetches, writes atomic.Int32
	raw := []byte("Message-ID: <fixture@example.invalid>\r\n\r\nfixture body\r\n")
	cfg := &config.Config{Sources: []config.SourceConfig{{Name: "source", Type: "fixture", Folders: []string{"INBOX"}}}, Targets: []config.TargetConfig{{Name: "archive", Type: "mbox"}}, Pairs: []config.PairConfig{{Source: "source", Targets: []string{"archive"}}}}
	engines := make([]*SyncEngine, 0, 2)
	for range 2 {
		store, err := state.NewStore(database)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		registry := plugins.NewRegistry()
		registry.RegisterSource("fixture", func(map[string]interface{}) (plugins.Source, error) {
			return &runLockSource{fixtureSource: fixtureSource{name: "source", messages: []*plugins.Message{{UID: "1", Folder: "INBOX", Raw: raw}}}, fetches: &fetches}, nil
		})
		registry.RegisterTarget("mbox", func(map[string]interface{}) (plugins.Target, error) {
			p, err := target.NewMboxTarget(map[string]interface{}{"path": mailbox, "create_if_missing": true})
			if err != nil {
				return nil, err
			}
			return &runLockTarget{MboxTarget: p.(*target.MboxTarget), writes: &writes, entered: entered, release: release}, nil
		})
		engine, err := NewSyncEngine(cfg, store, registry, WithMaxRetries(0))
		if err != nil {
			t.Fatal(err)
		}
		engines = append(engines, engine)
	}
	type outcome struct {
		stats *SyncStats
		err   error
	}
	firstDone := make(chan outcome, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { stats, err := engines[0].Run(ctx); firstDone <- outcome{stats, err} }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first run did not reach its write")
	}
	finished := false
	defer func() {
		if !finished {
			close(release)
			<-firstDone
		}
	}()
	waitingCtx, stop := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer stop()
	_, err := engines[1].Run(waitingCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("overlapping run did not wait for ownership: %v", err)
	}
	if fetches.Load() != 1 || writes.Load() != 1 {
		t.Fatalf("waiter performed work: fetches=%d writes=%d", fetches.Load(), writes.Load())
	}
	close(release)
	first := <-firstDone
	finished = true
	if first.err != nil {
		t.Fatal(first.err)
	}
	assertStats(t, first.stats, 1, 0, 0)
	second, err := engines[1].Run(context.Background())
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
