package main

import (
	"encoding/json"
	"strings"
	"testing"

	syncengine "github.com/zekihan/mailvault/internal/sync"
)

func TestJSONSyncFailureRetainsDetails(t *testing.T) {
	configureCLI(t)
	jsonOutput = true
	output, err := captureOutput(t, func() error { return runSync(nil, nil) })
	if err == nil {
		t.Fatal("failed sync returned success")
	}
	var doc struct {
		TotalErrors int
		Results     []struct{ Errors []string }
	}
	if err := json.Unmarshal([]byte(output), &doc); err != nil {
		t.Fatalf("invalid or uninformative JSON: %v", err)
	}
	if doc.TotalErrors != 1 || len(doc.Results) != 1 || len(doc.Results[0].Errors) != 1 || !strings.Contains(doc.Results[0].Errors[0], "connect") {
		t.Fatalf("missing source failure details: %s", output)
	}
}

func TestHumanStatsIncludeFinalizationErrors(t *testing.T) {
	configureCLI(t)
	jsonOutput = false
	var stats syncengine.SyncStats
	if err := json.Unmarshal([]byte(`{"TotalErrors":1,"FinalizationErrors":["closing target local: finalization fixture failure"]}`), &stats); err != nil {
		t.Fatal(err)
	}
	output, err := captureOutput(t, func() error { return printStats(&stats) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "finalization fixture failure") {
		t.Fatalf("missing finalization diagnostics: %s", output)
	}
}
