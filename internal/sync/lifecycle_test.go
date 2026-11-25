package sync

import (
	"errors"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

type resourceTarget struct {
	*fixtureTarget

	closes   int
	closeErr error
}

func (t *resourceTarget) Close() error { t.closes++; return t.closeErr }

type resourceSource struct {
	*fixtureSource

	closes int
}

func (s *resourceSource) Close() error { s.closes++; return nil }

func lifecycleFixture(t *testing.T) (*engineFixture, map[string]*resourceTarget, *resourceSource) {
	t.Helper()
	fixture := newEngineFixture(t)
	targets := map[string]*resourceTarget{}
	for name, target := range fixture.targets {
		targets[name] = &resourceTarget{fixtureTarget: target}
	}
	source := &resourceSource{fixtureSource: fixture.source}
	fixture.registry.RegisterTarget("fixture", func(values map[string]any) (plugins.Target, error) {
		return targets[mustAssert[string](t, values["name"])], nil
	})
	fixture.registry.RegisterSource("fixture", func(map[string]any) (plugins.Source, error) { return source, nil })
	return fixture, targets, source
}

func TestEngineClosesResourcesOnStateSetupFailure(t *testing.T) {
	fixture, targets, source := lifecycleFixture(t)
	engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = engine.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "upsert source") {
		t.Fatalf("expected state setup failure: %v", err)
	}
	for name, target := range targets {
		if target.initializations != 1 || target.closes != 1 {
			t.Fatalf("%s initialized=%d closed=%d", name, target.initializations, target.closes)
		}
	}
	if source.closes != 1 {
		t.Fatalf("source closed %d times", source.closes)
	}
}

func TestEngineClosesPartiallyInitializedTarget(t *testing.T) {
	fixture, targets, source := lifecycleFixture(t)
	fixture.cfg.Pairs[0].Targets = []string{"first"}
	targets["first"].initializeErr = errFixture
	_, err := fixture.runError(t)
	if !errors.Is(err, errFixture) {
		t.Fatalf("lost initialize failure: %v", err)
	}
	if targets["first"].closes != 1 || source.closes != 1 {
		t.Fatalf("partial initialization not cleaned: target=%d source=%d", targets["first"].closes, source.closes)
	}
}

func (fixture *engineFixture) runError(t *testing.T) (*SyncStats, error) {
	t.Helper()
	engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry)
	if err != nil {
		t.Fatal(err)
	}
	return engine.Run(t.Context())
}

func TestEnginePreservesSetupAndCleanupFailures(t *testing.T) {
	fixture, targets, _ := lifecycleFixture(t)
	fixture.cfg.Pairs[0].Targets = []string{"first"}
	initializeErr := errors.New("synthetic initialize failure")
	targets["first"].initializeErr = initializeErr
	targets["first"].closeErr = errFixture
	_, err := fixture.runError(t)
	if !errors.Is(err, initializeErr) || !errors.Is(err, errFixture) {
		t.Fatalf("lost setup or cleanup failure: %v", err)
	}
}
