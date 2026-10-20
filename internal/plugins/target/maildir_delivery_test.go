//go:build unix

package target

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/state"
	syncengine "github.com/zekihan/mailvault/internal/sync"
)

type durableFixtureSource struct{ message *plugins.Message }

func (*durableFixtureSource) Name() string                  { return "source" }
func (*durableFixtureSource) Type() string                  { return "fixture" }
func (*durableFixtureSource) Connect(context.Context) error { return nil }
func (*durableFixtureSource) Close() error                  { return nil }
func (*durableFixtureSource) ListFolders(context.Context) ([]string, error) {
	return []string{"INBOX"}, nil
}
func (s *durableFixtureSource) FetchMessages(context.Context, []string, int64, int64) (<-chan *plugins.Message, <-chan error) {
	messages := make(chan *plugins.Message, 1)
	messages <- s.message
	close(messages)
	failures := make(chan error)
	close(failures)
	return messages, failures
}

func TestMaildirFailedDurabilityDoesNotPersistDelivery(t *testing.T) {
	target := durableMaildirFixture(t)
	newDir := filepath.Join(target.path, "new")
	target.syncDir = func(path string) error {
		if path == newDir {
			entries, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			if len(entries) > 0 {
				return errMaildirSyncFixture
			}
		}
		return syncMaildirDirectory(path)
	}
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	message := &plugins.Message{UID: "1", Folder: "INBOX", Raw: []byte("Subject: fixture\r\n\r\nbody\r\n")}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]interface{}) (plugins.Source, error) {
		return &durableFixtureSource{message: message}, nil
	})
	registry.RegisterTarget("maildir", func(map[string]interface{}) (plugins.Target, error) { return target, nil })
	cfg := &config.Config{Sources: []config.SourceConfig{{Name: "source", Type: "fixture", Folders: []string{"INBOX"}}}, Targets: []config.TargetConfig{{Name: "archive", Type: "maildir"}}, Pairs: []config.PairConfig{{Source: "source", Targets: []string{"archive"}}}}
	run := func() *syncengine.SyncStats {
		engine, err := syncengine.NewSyncEngine(cfg, store, registry, syncengine.WithMaxRetries(0))
		if err != nil {
			t.Fatal(err)
		}
		stats, err := engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return stats
	}
	failed := run()
	if failed.TotalWritten != 0 || failed.TotalErrors != 1 {
		t.Fatalf("failed sync acknowledged delivery: %+v", failed)
	}
	identity := "hash:" + dedup.ComputeContentHash(message.Raw)
	exists, err := store.DeliveryExists(context.Background(), "archive", identity)
	if err != nil || exists {
		t.Fatalf("failed durability persisted receipt: %v %v", exists, err)
	}
	target.syncDir = nil
	success := run()
	if success.TotalWritten != 1 || success.TotalErrors != 0 {
		t.Fatalf("durable retry failed: %+v", success)
	}
	exists, err = store.DeliveryExists(context.Background(), "archive", identity)
	if err != nil || !exists {
		t.Fatalf("durable delivery missing receipt: %v %v", exists, err)
	}
}
