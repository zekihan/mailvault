package sync

import (
	"context"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

type observedSource struct{ *fixtureSource }

func (source *observedSource) FetchMessages(ctx context.Context, folders []string, since, until int64) (<-chan *plugins.Message, <-chan error) {
	err := plugins.ObserveFolderState(ctx, plugins.FolderStateObservation{Folder: "Archive", UIDValidity: "7", UIDNext: "43", ModSeq: "50"})
	if err != nil {
		messages := make(chan *plugins.Message)
		failures := make(chan error, 1)
		close(messages)
		failures <- err
		close(failures)
		return messages, failures
	}
	return source.fixtureSource.FetchMessages(ctx, folders, since, until)
}

func TestFolderObservationsAreNotWrittenByDryRuns(t *testing.T) {
	for _, dry := range []bool{false, true} {
		fixture := newEngineFixture(t)
		fixture.registry.RegisterSource("fixture", func(map[string]any) (plugins.Source, error) { return &observedSource{fixture.source}, nil })
		engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry, WithDryRun(dry))
		if err != nil {
			t.Fatal(err)
		}
		stats, err := engine.Run(t.Context())
		if err != nil || stats.TotalErrors != 0 {
			t.Fatalf("run: %+v %v", stats, err)
		}
		observed, err := fixture.store.GetFolderState(t.Context(), "account", "Archive")
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
