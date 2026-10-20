package sync

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestDryRunProgressReportsCounts(t *testing.T) {
	f := newEngineFixture(t)
	var mu sync.Mutex
	statuses := make(map[string]string)
	engine, err := NewSyncEngine(f.cfg, f.store, f.registry, WithDryRun(true), WithProgress(func(key, status string) {
		mu.Lock()
		defer mu.Unlock()
		statuses[key] = status
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"first", "second"} {
		status := statuses["account -> "+target]
		if !strings.Contains(status, "finished: 2 fetched, 1 would write, 1 skipped, 0 errors") {
			t.Fatalf("unexpected progress: %q", status)
		}
		if f.targets[target].writes != 0 {
			t.Fatal("dry run wrote messages")
		}
	}
}
