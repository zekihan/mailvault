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

type durableFixture struct {
	target   *MaildirTarget
	store    *state.Store
	message  *plugins.Message
	cfg      *config.Config
	registry *plugins.Registry
}

func (fixture *durableFixture) open(t *testing.T) {
	t.Helper()
	fixture.target = durableMaildirFixture(t)
	fixture.target.SetName("archive")
	fixture.installFailingSync(fixture.target)
	store, err := state.NewStore(t.Context(), filepath.Join(physicalTempDir(t), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = store
	fixture.message = &plugins.Message{UID: "1", Folder: "INBOX", Raw: []byte("Subject: fixture\r\n\r\nbody\r\n")}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]any) (plugins.Source, error) {
		return &durableFixtureSource{message: fixture.message}, nil
	})
	registry.RegisterTarget("maildir", func(map[string]any) (plugins.Target, error) { return fixture.target, nil })
	fixture.cfg = &config.Config{Sources: []config.SourceConfig{{Name: "source", Type: "fixture", Folders: []string{"INBOX"}}}, Targets: []config.TargetConfig{{Name: "archive", Type: "maildir"}}, Pairs: []config.PairConfig{{Source: "source", Targets: []string{"archive"}}}}
	fixture.registry = registry
}

func (fixture *durableFixture) installFailingSync(target *MaildirTarget) {
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
}

func (fixture *durableFixture) run(t *testing.T) *syncengine.SyncStats {
	t.Helper()
	engine, err := syncengine.NewSyncEngine(fixture.cfg, fixture.store, fixture.registry, syncengine.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := engine.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func (fixture *durableFixture) identity() string {
	return "placement:" + dedup.ComputeContentHash([]byte(fixture.target.DestinationIdentity()+"\x00hash:"+dedup.ComputeContentHash(fixture.message.Raw)))
}

func (fixture *durableFixture) checkDelivery(t *testing.T, want bool) {
	t.Helper()
	exists, err := fixture.store.DeliveryExists(t.Context(), plugins.TargetStateKey(fixture.target), fixture.identity())
	if err != nil || exists != want {
		t.Fatalf("delivery receipt=%v want %v: %v", exists, want, err)
	}
}

func TestMaildirFailedDurabilityDoesNotPersistDelivery(t *testing.T) {
	fixture := &durableFixture{}
	fixture.open(t)
	defer fixture.store.Close()
	failed := fixture.run(t)
	if failed.TotalWritten != 0 || failed.TotalErrors != 1 {
		t.Fatalf("failed sync acknowledged delivery: %+v", failed)
	}
	fixture.checkDelivery(t, false)
	fixture.target.syncDir = nil
	success := fixture.run(t)
	if success.TotalWritten != 1 || success.TotalErrors != 0 {
		t.Fatalf("durable retry failed: %+v", success)
	}
	fixture.checkDelivery(t, true)
}
