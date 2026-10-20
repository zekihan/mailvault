package sync

import (
	"context"
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
	f := newEngineFixture(t)
	f.source.messages = nil
	f.source.failure = fmt.Errorf("protocol read: %w", errFixture)
	stats := f.run(t)
	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Results []struct {
			SourceName, TargetName string
			Errors                 []string
		}
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

func TestFinalizationFailuresCountWithoutWritingStdout(t *testing.T) {
	f := newEngineFixture(t)
	f.registry.RegisterSource("fixture", func(map[string]interface{}) (plugins.Source, error) { return &closingSource{f.source}, nil })
	f.registry.RegisterTarget("fixture", func(values map[string]interface{}) (plugins.Target, error) {
		return &closingTarget{f.targets[values["name"].(string)]}, nil
	})
	e, err := NewSyncEngine(f.cfg, f.store, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = old; _ = file.Close() }()
	stats, runErr := e.Run(context.Background())
	if runErr != nil {
		t.Fatal(runErr)
	}
	if stats.TotalErrors != 3 || stats.TotalWritten != 2 || len(stats.Results) != 2 {
		t.Fatalf("finalization failures not counted separately from delivery results: %+v", stats)
	}
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
	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ FinalizationErrors []string }
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
