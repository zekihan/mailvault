package sync

import (
	"context"
	"testing"
)

func TestEngineSkipsUnusedTargets(t *testing.T) {
	f := newEngineFixture(t)
	f.cfg.Pairs[0].Targets = []string{"first"}
	f.targets["second"].initializeErr = errFixture
	assertStats(t, f.run(t), 1, 1, 0)
	if f.targets["second"].initializations != 0 {
		t.Fatal("unused target initialized")
	}
}
func TestEngineDryRunDoesNotInitializeOrMutateState(t *testing.T) {
	f := newEngineFixture(t)
	assertStats(t, f.run(t, WithDryRun(true)), 2, 2, 0)
	if f.targets["first"].initializations != 0 || f.targets["second"].initializations != 0 {
		t.Fatal("dry run initialized targets")
	}
	stats, err := f.store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats["sources"] != 0 || stats["messages"] != 0 {
		t.Fatal("dry run persisted source/message state")
	}
}
