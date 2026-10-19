package sync

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/state"
)

var errFixture = errors.New("synthetic failure")

type fixtureSource struct {
	name     string
	messages []*plugins.Message
	failure  error
}

func (s *fixtureSource) Name() string                                { return s.name }
func (*fixtureSource) Type() string                                  { return "fixture" }
func (*fixtureSource) Connect(context.Context) error                 { return nil }
func (*fixtureSource) ListFolders(context.Context) ([]string, error) { return []string{"Archive"}, nil }
func (*fixtureSource) Close() error                                  { return nil }
func (s *fixtureSource) FetchMessages(context.Context, []string, int64, int64) (<-chan *plugins.Message, <-chan error) {
	messages := make(chan *plugins.Message, len(s.messages))
	errs := make(chan error, 1)
	for _, msg := range s.messages {
		messages <- msg
	}
	if s.failure != nil {
		errs <- s.failure
	}
	close(messages)
	close(errs)
	return messages, errs
}

type fixtureTarget struct {
	name   string
	writes int
	fail   bool
}

func (t *fixtureTarget) Name() string                   { return t.name }
func (*fixtureTarget) Type() string                     { return "fixture" }
func (*fixtureTarget) Initialize(context.Context) error { return nil }
func (*fixtureTarget) Close() error                     { return nil }
func (t *fixtureTarget) WriteMessage(context.Context, *plugins.Message) error {
	if t.fail {
		return errFixture
	}
	t.writes++
	return nil
}

type engineFixture struct {
	cfg      *config.Config
	store    *state.Store
	registry *plugins.Registry
	source   *fixtureSource
	targets  map[string]*fixtureTarget
}

func newEngineFixture(t *testing.T) *engineFixture {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	raw := []byte("Message-ID: <fixture@example.invalid>\r\n\r\nbody\r\n")
	msg := &plugins.Message{UID: "42", Folder: "Archive", MessageID: "fixture@example.invalid", Raw: raw, ContentHash: dedup.ComputeContentHash(raw)}
	f := &engineFixture{store: store, registry: plugins.NewRegistry(), source: &fixtureSource{name: "account", messages: []*plugins.Message{msg, msg}}, targets: map[string]*fixtureTarget{"first": {name: "first"}, "second": {name: "second"}}}
	f.cfg = &config.Config{Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"Archive"}}}, Targets: []config.TargetConfig{{Name: "first", Type: "fixture"}, {Name: "second", Type: "fixture"}}, Pairs: []config.PairConfig{{Source: "account", Targets: []string{"first", "second"}}}}
	f.registry.RegisterSource("fixture", func(map[string]interface{}) (plugins.Source, error) { return f.source, nil })
	f.registry.RegisterTarget("fixture", func(values map[string]interface{}) (plugins.Target, error) {
		return f.targets[values["name"].(string)], nil
	})
	return f
}
func (f *engineFixture) run(t *testing.T, opts ...SyncOption) *SyncStats {
	t.Helper()
	opts = append([]SyncOption{WithMaxRetries(0)}, opts...)
	e, err := NewSyncEngine(f.cfg, f.store, f.registry, opts...)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := e.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return stats
}
func assertStats(t *testing.T, s *SyncStats, written, skipped, failures int) {
	t.Helper()
	if s.TotalWritten != written || s.TotalSkipped != skipped || s.TotalErrors != failures {
		t.Fatalf("stats=%+v, want written=%d skipped=%d errors=%d", s, written, skipped, failures)
	}
}

func TestEngineDeliversEveryTargetAndPersistsPerTarget(t *testing.T) {
	f := newEngineFixture(t)
	if err := f.store.UpsertSource(context.Background(), "account", "fixture", "", ""); err != nil {
		t.Fatal(err)
	}
	msg := f.source.messages[0]
	if err := f.store.MarkMessageFetched(context.Background(), "account", msg.Folder, msg.UID, msg.MessageID, msg.ContentHash, msg.Size, msg.InternalDate); err != nil {
		t.Fatal(err)
	}
	assertStats(t, f.run(t), 2, 2, 0) // A legacy fetched row cannot prove delivery to either target.
	if f.targets["first"].writes != 1 || f.targets["second"].writes != 1 {
		t.Fatal("missing fanout delivery")
	}
	assertStats(t, f.run(t), 0, 4, 0)
	f.targets["third"] = &fixtureTarget{name: "third"}
	f.cfg.Targets = append(f.cfg.Targets, config.TargetConfig{Name: "third", Type: "fixture"})
	f.cfg.Pairs[0].Targets = append(f.cfg.Pairs[0].Targets, "third")
	assertStats(t, f.run(t), 1, 5, 0)
	if f.targets["third"].writes != 1 {
		t.Fatal("new destination did not receive existing message")
	}
}
func TestEngineRetriesFailedTargetWithoutSuppressingOtherTargets(t *testing.T) {
	f := newEngineFixture(t)
	f.source.messages = f.source.messages[:1]
	f.targets["second"].fail = true
	assertStats(t, f.run(t), 1, 0, 1)
	f.targets["second"].fail = false
	assertStats(t, f.run(t), 1, 1, 0)
}
func TestEngineDryRunChecksStateWithoutPersisting(t *testing.T) {
	f := newEngineFixture(t)
	assertStats(t, f.run(t, WithDryRun(true)), 2, 2, 0)
	if f.targets["first"].writes != 0 || f.targets["second"].writes != 0 {
		t.Fatal("dry run wrote to target")
	}
	assertStats(t, f.run(t), 2, 2, 0)
	assertStats(t, f.run(t, WithDryRun(true)), 0, 4, 0)
}
func TestEngineDrainsBufferedSourceErrors(t *testing.T) {
	f := newEngineFixture(t)
	f.source.messages = nil
	f.source.failure = errFixture
	for i := 0; i < 20; i++ {
		assertStats(t, f.run(t), 0, 0, 2)
	}
}
func TestEngineSerializesSameTargetAcrossSources(t *testing.T) {
	f := newEngineFixture(t)
	f.cfg.Pairs = append(f.cfg.Pairs, f.cfg.Pairs[0])
	assertStats(t, f.run(t), 2, 6, 0)
}
func TestEngineRejectsInvalidOptions(t *testing.T) {
	f := newEngineFixture(t)
	for _, opt := range []SyncOption{WithConcurrency(0), WithConcurrency(-1), WithMaxRetries(-1), WithRetryDelay(-1)} {
		if _, err := NewSyncEngine(f.cfg, f.store, f.registry, opt); err == nil {
			t.Fatal("invalid option accepted")
		}
	}
}
func TestDedupTrackerConcurrentAccess(t *testing.T) {
	tracker := dedup.NewDedupTracker(nil, "account")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				msg := &plugins.Message{MessageID: "same"}
				_, _ = tracker.ShouldSkip(context.Background(), msg, "Archive")
				tracker.MarkSeen(msg)
				_ = tracker.SeenCount()
			}
		}()
	}
	wg.Wait()
	if tracker.SeenCount() != 1 {
		t.Fatal("unexpected seen count")
	}
}
