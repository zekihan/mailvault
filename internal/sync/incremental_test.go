package sync

import (
	"context"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestIMAPReceiptsRequireSuccessfulDelivery(t *testing.T) {
	for _, mode := range []string{"success", "failure", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			f := newEngineFixture(t)
			for _, msg := range f.source.messages {
				msg.UIDScope = "generation-1"
			}
			f.targets["first"].fail = mode == "failure"
			engine, err := NewSyncEngine(f.cfg, f.store, f.registry, WithDryRun(mode == "dry-run"), WithMaxRetries(0))
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if _, err := engine.Run(ctx); err != nil {
				t.Fatal(err)
			}
			for _, target := range []string{"first", "second"} {
				known, err := f.store.KnownUIDs(ctx, "account", target, "generation-1", "Archive")
				if err != nil {
					t.Fatal(err)
				}
				want := mode != "dry-run" && (mode != "failure" || target != "first")
				if known["42"] != want {
					t.Fatalf("%s receipt = %v, want %v", target, known, want)
				}
			}
			for _, query := range [][4]string{{"account", "new-target", "generation-1", "Archive"}, {"account", "first", "generation-2", "Archive"}, {"account", "first", "generation-1", "Other"}} {
				known, err := f.store.KnownUIDs(ctx, query[0], query[1], query[2], query[3])
				if err != nil || len(known) != 0 {
					t.Fatalf("receipt leaked across scope: %v %v", known, err)
				}
			}
		})
	}
}

// incrementalFixtureSource exercises the source/engine receipt contract without a network account.
type incrementalFixtureSource struct {
	*fixtureSource
}

func (s *incrementalFixtureSource) FetchMessages(ctx context.Context, folders []string, since, until int64) (<-chan *plugins.Message, <-chan error) {
	messages := make(chan *plugins.Message, len(s.messages))
	failures := make(chan error, 1)
	defer close(messages)
	defer close(failures)
	known, err := plugins.KnownUIDs(ctx, "generation-1", "Archive")
	if err != nil {
		failures <- err
		return messages, failures
	}
	for _, msg := range s.messages {
		if !known[msg.UID] {
			messages <- msg
		}
	}
	return messages, failures
}

func TestIncrementalSecondRunAndForceFetchAll(t *testing.T) {
	f := newEngineFixture(t)
	f.cfg.Pairs = f.cfg.Pairs[:1]
	f.cfg.Pairs[0].Targets = []string{"first"}
	f.source.messages = f.source.messages[:1]
	f.source.messages[0].UIDScope = "generation-1"
	f.registry.RegisterSource("fixture", func(map[string]interface{}) (plugins.Source, error) {
		return &incrementalFixtureSource{f.source}, nil
	})
	run := func(force bool, wantFetched int) {
		t.Helper()
		engine, err := NewSyncEngine(f.cfg, f.store, f.registry, WithForceFetchAll(force))
		if err != nil {
			t.Fatal(err)
		}
		stats, err := engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if stats.TotalErrors != 0 || stats.TotalFetched != wantFetched {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	}
	run(false, 1)
	run(false, 0)
	f.source.messages = append(f.source.messages, &plugins.Message{UID: "43", UIDScope: "generation-1", Folder: "Archive", Raw: []byte("Subject: new\r\n\r\nnew body")})
	run(false, 1)
	run(true, 2)
	if f.targets["first"].writes != 2 {
		t.Fatalf("force fetch duplicated backups: %d writes", f.targets["first"].writes)
	}
}
