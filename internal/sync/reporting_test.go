package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestJSONResultsRetainErrorMessages(t *testing.T) {
	fixture := newEngineFixture(t)
	fixture.source.messages = nil
	fixture.source.failure = fmt.Errorf("protocol read: %w", errFixture)
	stats := fixture.run(t)
	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Results []struct {
			SourceName string   `json:"SourceName"`
			TargetName string   `json:"TargetName"`
			Errors     []string `json:"Errors"`
		} `json:"Results"`
	}
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatalf("errors are not JSON strings: %v", err)
	}
	if len(doc.Results) != 2 {
		t.Fatal("lost results")
	}
	for i, result := range doc.Results {
		if result.SourceName != "account" || result.TargetName == "" || len(result.Errors) != 1 || result.Errors[0] != "protocol read: synthetic failure" {
			t.Fatalf("lost error details: %+v", result)
		}
		if !errors.Is(stats.Results[i].Errors[0], errFixture) {
			t.Fatal("serialization changed operational error values")
		}
	}
}

type closingTarget struct{ *fixtureTarget }

func (*closingTarget) Close() error { return errFixture }

type closingSource struct{ *fixtureSource }

func (*closingSource) Close() error { return errFixture }

func setupClosingFixture(t *testing.T) *engineFixture {
	t.Helper()
	fixture := newEngineFixture(t)
	fixture.registry.RegisterSource("fixture", func(map[string]any) (plugins.Source, error) { return &closingSource{fixture.source}, nil })
	fixture.registry.RegisterTarget("fixture", func(values map[string]any) (plugins.Target, error) {
		return &closingTarget{fixture.targets[mustAssert[string](t, values["name"])]}, nil
	})
	return fixture
}

func captureRunStdout(t *testing.T, engine *SyncEngine) (*os.File, *SyncStats, error) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	old := os.Stdout
	os.Stdout = file
	stats, runErr := engine.Run(t.Context())
	os.Stdout = old
	return file, stats, runErr
}

func assertFinalizationStats(t *testing.T, stats *SyncStats, runErr error) {
	t.Helper()
	if runErr != nil {
		t.Fatal(runErr)
	}
	if stats.TotalErrors != 3 || stats.TotalWritten != 2 || len(stats.Results) != 2 {
		t.Fatalf("finalization failures not counted separately from delivery results: %+v", stats)
	}
}

func assertStdoutEmpty(t *testing.T, file *os.File) {
	t.Helper()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 0 {
		t.Fatalf("Run wrote diagnostics to stdout: %s", output)
	}
}

func assertFinalizationDiagnostics(t *testing.T, stats *SyncStats) {
	t.Helper()
	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		FinalizationErrors []string `json:"FinalizationErrors"`
	}
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.FinalizationErrors) != 3 {
		t.Fatalf("missing finalization diagnostics: %s", encoded)
	}
	joined := strings.Join(doc.FinalizationErrors, "\n")
	for _, name := range []string{`target "first"`, `target "second"`, `source "account"`} {
		if !strings.Contains(joined, name) {
			t.Fatalf("missing %s: %s", name, joined)
		}
	}
}

func TestFinalizationFailuresCountWithoutWritingStdout(t *testing.T) {
	fixture := setupClosingFixture(t)
	engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry)
	if err != nil {
		t.Fatal(err)
	}
	file, stats, runErr := captureRunStdout(t, engine)
	assertFinalizationStats(t, stats, runErr)
	assertStdoutEmpty(t, file)
	assertFinalizationDiagnostics(t, stats)
}
