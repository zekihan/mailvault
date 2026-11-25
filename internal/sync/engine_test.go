package sync

import (
	"context"
	"errors"
	"path/filepath"
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

func (source *fixtureSource) Name() string                           { return source.name }
func (*fixtureSource) Type() string                                  { return "fixture" }
func (*fixtureSource) Connect(context.Context) error                 { return nil }
func (*fixtureSource) ListFolders(context.Context) ([]string, error) { return []string{"Archive"}, nil }
func (*fixtureSource) Close() error                                  { return nil }
func (source *fixtureSource) FetchMessages(context.Context, []string, int64, int64) (<-chan *plugins.Message, <-chan error) {
	messages := make(chan *plugins.Message, len(source.messages))
	errs := make(chan error, 1)
	for _, msg := range source.messages {
		messages <- msg
	}
	if source.failure != nil {
		errs <- source.failure
	}
	close(messages)
	close(errs)
	return messages, errs
}

type fixtureTarget struct {
	name            string
	writes          int
	fail            bool
	initializations int
	initializeErr   error
}

func (t *fixtureTarget) Name() string { return t.name }
func (*fixtureTarget) Type() string   { return "fixture" }
func (t *fixtureTarget) Initialize(context.Context) error {
	t.initializations++
	return t.initializeErr
}
func (*fixtureTarget) Close() error { return nil }
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
	store, err := state.NewStore(t.Context(), filepath.Join(physicalTempDir(t), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	raw := []byte("Message-ID: <fixture@example.invalid>\r\n\r\nbody\r\n")
	msg := &plugins.Message{UID: "42", Folder: "Archive", MessageID: "fixture@example.invalid", Raw: raw, ContentHash: dedup.ComputeContentHash(raw)}
	fixture := &engineFixture{store: store, registry: plugins.NewRegistry(), source: &fixtureSource{name: "account", messages: []*plugins.Message{msg, msg}}, targets: map[string]*fixtureTarget{"first": {name: "first"}, "second": {name: "second"}}}
	fixture.cfg = &config.Config{Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"Archive"}}}, Targets: []config.TargetConfig{{Name: "first", Type: "fixture"}, {Name: "second", Type: "fixture"}}, Pairs: []config.PairConfig{{Source: "account", Targets: []string{"first", "second"}}}}
	fixture.registry.RegisterSource("fixture", func(map[string]any) (plugins.Source, error) { return fixture.source, nil })
	fixture.registry.RegisterTarget("fixture", func(values map[string]any) (plugins.Target, error) {
		return fixture.targets[mustAssert[string](t, values["name"])], nil
	})
	return fixture
}
func (fixture *engineFixture) run(t *testing.T, opts ...SyncOption) *SyncStats {
	t.Helper()
	opts = append([]SyncOption{WithMaxRetries(0)}, opts...)
	engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry, opts...)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := engine.Run(t.Context())
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
	fixture := newEngineFixture(t)
	if err := fixture.store.UpsertSource(t.Context(), "account", "fixture", "", ""); err != nil {
		t.Fatal(err)
	}
	msg := fixture.source.messages[0]
	if err := fixture.store.MarkMessageFetched(t.Context(), "account", msg.Folder, msg.UID, msg.MessageID, msg.ContentHash, msg.Size, msg.InternalDate); err != nil {
		t.Fatal(err)
	}
	assertStats(t, fixture.run(t), 2, 2, 0) // A legacy fetched row cannot prove delivery to either target.
	if fixture.targets["first"].writes != 1 || fixture.targets["second"].writes != 1 {
		t.Fatal("missing fanout delivery")
	}
	assertStats(t, fixture.run(t), 0, 4, 0)
	fixture.targets["third"] = &fixtureTarget{name: "third"}
	fixture.cfg.Targets = append(fixture.cfg.Targets, config.TargetConfig{Name: "third", Type: "fixture"})
	fixture.cfg.Pairs[0].Targets = append(fixture.cfg.Pairs[0].Targets, "third")
	assertStats(t, fixture.run(t), 1, 5, 0)
	if fixture.targets["third"].writes != 1 {
		t.Fatal("new destination did not receive existing message")
	}
}
func TestEngineRetriesFailedTargetWithoutSuppressingOtherTargets(t *testing.T) {
	fixture := newEngineFixture(t)
	fixture.source.messages = fixture.source.messages[:1]
	fixture.targets["second"].fail = true
	assertStats(t, fixture.run(t), 1, 0, 1)
	fixture.targets["second"].fail = false
	assertStats(t, fixture.run(t), 1, 1, 0)
}
func TestEngineDryRunChecksStateWithoutPersisting(t *testing.T) {
	fixture := newEngineFixture(t)
	assertStats(t, fixture.run(t, WithDryRun(true)), 2, 2, 0)
	if fixture.targets["first"].writes != 0 || fixture.targets["second"].writes != 0 {
		t.Fatal("dry run wrote to target")
	}
	assertStats(t, fixture.run(t), 2, 2, 0)
	assertStats(t, fixture.run(t, WithDryRun(true)), 0, 4, 0)
}
func TestEngineDrainsBufferedSourceErrors(t *testing.T) {
	fixture := newEngineFixture(t)
	fixture.source.messages = nil
	fixture.source.failure = errFixture
	for range 20 {
		assertStats(t, fixture.run(t), 0, 0, 2)
	}
}
func TestEngineSerializesSameTargetAcrossSources(t *testing.T) {
	fixture := newEngineFixture(t)
	fixture.cfg.Pairs = append(fixture.cfg.Pairs, fixture.cfg.Pairs[0])
	assertStats(t, fixture.run(t), 2, 6, 0)
}
func TestEngineRejectsInvalidOptions(t *testing.T) {
	fixture := newEngineFixture(t)
	for _, opt := range []SyncOption{WithConcurrency(0), WithConcurrency(-1), WithMaxRetries(-1), WithRetryDelay(-1)} {
		if _, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry, opt); err == nil {
			t.Fatal("invalid option accepted")
		}
	}
}
