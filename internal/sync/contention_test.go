package sync

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/state"
)

type contentionSource struct {
	*incrementalFixtureSource

	entered chan<- struct{}
	release <-chan struct{}
}

func (source *contentionSource) FetchMessages(ctx context.Context, folders []string, since, until int64) (<-chan *plugins.Message, <-chan error) {
	if source.entered != nil {
		source.entered <- struct{}{}
		select {
		case <-source.release:
		case <-ctx.Done():
		}
	}
	err := plugins.ObserveFolderState(ctx, plugins.FolderStateObservation{Folder: "Archive", UIDValidity: "1", UIDNext: "2"})
	if err != nil {
		messages, failures := make(chan *plugins.Message), make(chan error, 1)
		close(messages)
		failures <- fmt.Errorf("record folder state: %w", err)
		close(failures)
		return messages, failures
	}
	return source.incrementalFixtureSource.FetchMessages(ctx, folders, since, until)
}

type contentionFixture struct {
	engine  *SyncEngine
	sources map[string]*contentionSource
	targets map[string]*fixtureTarget
	entered chan struct{}
	release chan struct{}
	store   *state.Store
	holder  *sql.Conn
}

type runOutcome struct {
	stats *SyncStats
	err   error
}

func setupContentionFixture(ctx context.Context, t *testing.T) *contentionFixture {
	t.Helper()
	path := filepath.Join(physicalTempDir(t), "state.db")
	store, err := state.NewStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	fixture := &contentionFixture{
		sources: map[string]*contentionSource{},
		targets: map[string]*fixtureTarget{},
		entered: make(chan struct{}, 3),
		release: make(chan struct{}),
		store:   store,
		holder:  holder,
	}
	registry := plugins.NewRegistry()
	cfg := &config.Config{}
	for i := range 3 {
		name := fmt.Sprintf("source-%d", i)
		target := fmt.Sprintf("target-%d", i)
		cfg.Sources = append(cfg.Sources, config.SourceConfig{Name: name, Type: "fixture", Folders: []string{"Archive"}})
		cfg.Targets = append(cfg.Targets, config.TargetConfig{Name: target, Type: "fixture"})
		cfg.Pairs = append(cfg.Pairs, config.PairConfig{Source: name, Targets: []string{target}})
		fixture.sources[name] = &contentionSource{incrementalFixtureSource: &incrementalFixtureSource{&fixtureSource{name: name, messages: []*plugins.Message{{UID: "1", UIDScope: "generation-1", Folder: "Archive", Raw: []byte("Subject: " + name + "\r\n\r\nfixture")}}}}, entered: fixture.entered, release: fixture.release}
		fixture.targets[target] = &fixtureTarget{name: target}
	}
	registry.RegisterSource("fixture", func(v map[string]any) (plugins.Source, error) {
		return fixture.sources[mustAssert[string](t, v["name"])], nil
	})
	registry.RegisterTarget("fixture", func(v map[string]any) (plugins.Target, error) {
		return fixture.targets[mustAssert[string](t, v["name"])], nil
	})
	fixture.engine, err = NewSyncEngine(cfg, store, registry, WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func startContentionRun(ctx context.Context, fixture *contentionFixture) chan runOutcome {
	done := make(chan runOutcome, 1)
	go func() { stats, err := fixture.engine.Run(ctx); done <- runOutcome{stats, err} }()
	return done
}

func waitContentionEntered(ctx context.Context, t *testing.T, fixture *contentionFixture, done chan runOutcome) {
	t.Helper()
	for range 3 {
		select {
		case <-fixture.entered:
		case result := <-done:
			t.Fatalf("early run completion: %+v", result)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func holdContentionLock(ctx context.Context, t *testing.T, fixture *contentionFixture) {
	t.Helper()
	if _, err := fixture.holder.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = fixture.holder.ExecContext(ctx, "ROLLBACK") })
	close(fixture.release)
}

func assertContentionOutcome(t *testing.T, result runOutcome, permanent bool) {
	t.Helper()
	if result.err != nil {
		t.Fatal(result.err)
	}
	if !permanent {
		assertStats(t, result.stats, 3, 0, 0)
		return
	}
	assertStats(t, result.stats, 0, 0, 3)
	for _, pair := range result.stats.Results {
		if len(pair.Errors) == 0 || !strings.Contains(fmt.Sprint(pair.Errors), "database is locked") {
			t.Fatalf("missing lock error: %+v", pair)
		}
	}
}

func rollbackContention(ctx context.Context, t *testing.T, fixture *contentionFixture) {
	t.Helper()
	if _, err := fixture.holder.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
}

func assertContentionFolderState(ctx context.Context, t *testing.T, fixture *contentionFixture, sources map[string]*contentionSource, permanent bool) {
	t.Helper()
	for name, source := range sources {
		source.entered = nil
		fs, err := fixture.store.GetFolderState(ctx, name, "Archive")
		if err != nil {
			t.Fatal(err)
		}
		if permanent && fs != nil {
			t.Fatalf("failed observation persisted: %+v", fs)
		}
		if !permanent && (fs == nil || fs.UIDNext != "2") {
			t.Fatalf("missing observation: %+v", fs)
		}
	}
}

func assertContentionReruns(ctx context.Context, t *testing.T, fixture *contentionFixture, permanent bool) {
	t.Helper()
	// A failed run leaves every UID pending; a successful one fetches none.
	next, err := fixture.engine.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	if permanent {
		want = 3
	}
	assertStats(t, next, want, 0, 0)
	if next.TotalFetched != want {
		t.Fatalf("pending work: %+v", next)
	}
	again, err := fixture.engine.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertStats(t, again, 0, 0, 0)
	if again.TotalFetched != 0 {
		t.Fatalf("receipts did not suppress repeat fetch: %+v", again)
	}
}

func assertContentionReceipts(ctx context.Context, t *testing.T, fixture *contentionFixture) {
	t.Helper()
	for i := range 3 {
		name, target := fmt.Sprintf("source-%d", i), fmt.Sprintf("target-%d", i)
		if fixture.targets[target].writes != 1 {
			t.Fatalf("duplicate or missing delivery: %+v", fixture.targets[target])
		}
		known, err := fixture.store.KnownUIDs(ctx, name, target, "generation-1", "Archive")
		if err != nil || !known["1"] {
			t.Fatalf("missing receipt: %v, %v", known, err)
		}
		exists, err := fixture.store.MessageExists(ctx, name, "Archive", "1")
		if err != nil || !exists {
			t.Fatalf("missing message: %v, %v", exists, err)
		}
	}
}

func runContentionCase(t *testing.T, permanent bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	fixture := setupContentionFixture(ctx, t)
	done := startContentionRun(ctx, fixture)
	waitContentionEntered(ctx, t, fixture, done)
	holdContentionLock(ctx, t, fixture)
	if !permanent {
		select {
		case result := <-done:
			t.Fatalf("run bypassed lock: %+v", result)
		case <-time.After(200 * time.Millisecond):
		}
		rollbackContention(ctx, t, fixture)
	}
	result := <-done
	assertContentionOutcome(t, result, permanent)
	if permanent {
		rollbackContention(ctx, t, fixture)
	}
	assertContentionFolderState(ctx, t, fixture, fixture.sources, permanent)
	assertContentionReruns(ctx, t, fixture, permanent)
	assertContentionReceipts(ctx, t, fixture)
}

func TestConcurrentSourcesWithSQLiteContention(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(fmt.Sprintf("permanent=%v", permanent), func(t *testing.T) {
			runContentionCase(t, permanent)
		})
	}
}
