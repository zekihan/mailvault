package sync

import (
	"strings"
	"sync"
	"testing"
)

func TestDryRunProgressReportsCounts(t *testing.T) {
	fixture := newEngineFixture(t)
	var mu sync.Mutex
	statuses := make(map[string]string)
	engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry, WithDryRun(true), WithProgress(func(key, status string) {
		mu.Lock()
		defer mu.Unlock()
		statuses[key] = status
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"first", "second"} {
		status := statuses["account -> "+target]
		if !strings.Contains(status, "finished: 2 fetched, 1 would write, 1 skipped, 0 errors") {
			t.Fatalf("unexpected progress: %q", status)
		}
		if fixture.targets[target].writes != 0 {
			t.Fatal("dry run wrote messages")
		}
	}
}
