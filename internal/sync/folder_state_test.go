package sync

import (
	"context"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

type observedSource struct{ *fixtureSource }

func (s *observedSource) FetchMessages(ctx context.Context, folders []string, since, until int64) (<-chan *plugins.Message, <-chan error) {
	err := plugins.ObserveFolderState(ctx, plugins.FolderStateObservation{Folder: "Archive", UIDValidity: "7", UIDNext: "43", ModSeq: "50"})
	if err != nil {
		messages := make(chan *plugins.Message)
		failures := make(chan error, 1)
		close(messages)
		failures <- err
		close(failures)
		return messages, failures
	}
	return s.fixtureSource.FetchMessages(ctx, folders, since, until)
}

func TestFolderObservationsAreNotWrittenByDryRuns(t *testing.T) {
	for _, dry := range []bool{false, true} {
		f := newEngineFixture(t)
		f.registry.RegisterSource("fixture", func(map[string]interface{}) (plugins.Source, error) { return &observedSource{f.source}, nil })
		engine, err := NewSyncEngine(f.cfg, f.store, f.registry, WithDryRun(dry))
		if err != nil {
			t.Fatal(err)
		}
		stats, err := engine.Run(context.Background())
		if err != nil || stats.TotalErrors != 0 {
			t.Fatalf("run: %+v %v", stats, err)
		}
		observed, err := f.store.GetFolderState(context.Background(), "account", "Archive")
		if err != nil {
			t.Fatal(err)
		}
		if dry {
			if observed != nil {
				t.Fatal("dry run persisted folder observations")
			}
		} else if observed == nil || observed.UIDValidity != "7" || observed.UIDNext != "43" || observed.ModSeq != "50" {
			t.Fatalf("missing folder observations: %+v", observed)
		}
	}
}
