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

func (s *contentionSource) FetchMessages(ctx context.Context, folders []string, since, until int64) (<-chan *plugins.Message, <-chan error) {
	if s.entered != nil {
		s.entered <- struct{}{}
		select {
		case <-s.release:
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
	return s.incrementalFixtureSource.FetchMessages(ctx, folders, since, until)
}

func TestConcurrentSourcesWithSQLiteContention(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(fmt.Sprintf("permanent=%v", permanent), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "state.db")
			store, err := state.NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			holder, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			entered, release := make(chan struct{}, 3), make(chan struct{})
			cfg := &config.Config{}
			registry := plugins.NewRegistry()
			sources := map[string]*contentionSource{}
			targets := map[string]*fixtureTarget{}
			for i := 0; i < 3; i++ {
				name := fmt.Sprintf("source-%d", i)
				target := fmt.Sprintf("target-%d", i)
				cfg.Sources = append(cfg.Sources, config.SourceConfig{Name: name, Type: "fixture", Folders: []string{"Archive"}})
				cfg.Targets = append(cfg.Targets, config.TargetConfig{Name: target, Type: "fixture"})
				cfg.Pairs = append(cfg.Pairs, config.PairConfig{Source: name, Targets: []string{target}})
				sources[name] = &contentionSource{incrementalFixtureSource: &incrementalFixtureSource{&fixtureSource{name: name, messages: []*plugins.Message{{UID: "1", UIDScope: "generation-1", Folder: "Archive", Raw: []byte("Subject: " + name + "\r\n\r\nfixture")}}}}, entered: entered, release: release}
				targets[target] = &fixtureTarget{name: target}
			}
			registry.RegisterSource("fixture", func(v map[string]interface{}) (plugins.Source, error) { return sources[v["name"].(string)], nil })
			registry.RegisterTarget("fixture", func(v map[string]interface{}) (plugins.Target, error) { return targets[v["name"].(string)], nil })
			engine, err := NewSyncEngine(cfg, store, registry, WithMaxRetries(0))
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				stats *SyncStats
				err   error
			}
			done := make(chan outcome, 1)
			go func() { stats, err := engine.Run(ctx); done <- outcome{stats, err} }()
			for range 3 {
				select {
				case <-entered:
				case result := <-done:
					t.Fatalf("early run completion: %+v", result)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if _, err := holder.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = holder.ExecContext(context.Background(), "ROLLBACK") }()
			close(release)
			if !permanent {
				select {
				case result := <-done:
					t.Fatalf("run bypassed lock: %+v", result)
				case <-time.After(200 * time.Millisecond):
				}
				if _, err := holder.ExecContext(ctx, "ROLLBACK"); err != nil {
					t.Fatal(err)
				}
			}
			result := <-done
			if result.err != nil {
				t.Fatal(result.err)
			}
			if permanent {
				assertStats(t, result.stats, 0, 0, 3)
				for _, pair := range result.stats.Results {
					if len(pair.Errors) == 0 || !strings.Contains(fmt.Sprint(pair.Errors), "database is locked") {
						t.Fatalf("missing lock error: %+v", pair)
					}
				}
				if _, err := holder.ExecContext(ctx, "ROLLBACK"); err != nil {
					t.Fatal(err)
				}
			} else {
				assertStats(t, result.stats, 3, 0, 0)
			}
			for name, source := range sources {
				source.entered = nil
				fs, err := store.GetFolderState(ctx, name, "Archive")
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
			// A failed run leaves every UID pending; a successful one fetches none.
			next, err := engine.Run(ctx)
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
			again, err := engine.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			assertStats(t, again, 0, 0, 0)
			if again.TotalFetched != 0 {
				t.Fatalf("receipts did not suppress repeat fetch: %+v", again)
			}
			for i := 0; i < 3; i++ {
				name, target := fmt.Sprintf("source-%d", i), fmt.Sprintf("target-%d", i)
				if targets[target].writes != 1 {
					t.Fatalf("duplicate or missing delivery: %+v", targets[target])
				}
				known, err := store.KnownUIDs(ctx, name, target, "generation-1", "Archive")
				if err != nil || !known["1"] {
					t.Fatalf("missing receipt: %v, %v", known, err)
				}
				exists, err := store.MessageExists(ctx, name, "Archive", "1")
				if err != nil || !exists {
					t.Fatalf("missing message: %v, %v", exists, err)
				}
			}
		})
	}
}
