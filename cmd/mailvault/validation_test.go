package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateConfigJSONWithoutConnectivity(t *testing.T) {
	configureCLI(t)
	jsonOutput = true
	dryRun = true
	output, err := captureOutput(t, func() error { return runValidateConfig(nil, nil) })
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Valid              bool `json:"valid"`
		ConnectivityTested bool `json:"connectivity_tested"`
		Sources            []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"sources"`
		Targets []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"targets"`
		Pairs  int      `json:"pairs"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("validation output is not JSON: %v", err)
	}
	if !report.Valid || report.ConnectivityTested || len(report.Sources) != 1 || len(report.Targets) != 1 || report.Pairs != 1 || len(report.Errors) != 0 {
		t.Fatalf("unexpected report: %s", output)
	}
	if report.Sources[0].Name != "test-account" || report.Sources[0].Status != "not_tested" {
		t.Fatalf("missing source details: %s", output)
	}
	if strings.Contains(output, "synthetic-sensitive-value") || strings.Contains(output, "synthetic-oauth-value") {
		t.Fatal("JSON validation exposed a credential")
	}
}

func TestValidateConfigJSONConnectivityFailure(t *testing.T) {
	configureCLI(t)
	jsonOutput = true
	output, err := captureOutput(t, func() error { return runValidateConfig(nil, nil) })
	if err == nil {
		t.Fatal("failed connectivity returned success")
	}
	var report struct {
		Valid              bool `json:"valid"`
		ConnectivityTested bool `json:"connectivity_tested"`
		Sources            []struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"sources"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("failed validation is not JSON: %v", err)
	}
	if report.Valid || !report.ConnectivityTested || len(report.Errors) == 0 || len(report.Sources) != 1 || report.Sources[0].Status != "failed" || !strings.Contains(report.Sources[0].Error, "connect") {
		t.Fatalf("missing failure detail: %s", output)
	}
}

func TestValidateConfigJSONStructuralFailure(t *testing.T) {
	configureCLI(t)
	jsonOutput = true
	if err := os.WriteFile(cfgFile, []byte(`{"sources":[],"targets":[],"pairs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := captureOutput(t, func() error { return runValidateConfig(nil, nil) })
	if err == nil {
		t.Fatal("invalid configuration accepted")
	}
	var report struct {
		Valid  bool     `json:"valid"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("structural failure is not JSON: %v", err)
	}
	if report.Valid || len(report.Errors) == 0 {
		t.Fatalf("missing structural error: %s", output)
	}
}

func TestValidateConfigRejectsInvalidOAuthEndpointBeforeConnectivity(t *testing.T) {
	configureCLI(t)
	jsonOutput = true
	targetPath := filepath.Join(physicalTempDir(t), "mail")
	config := fmt.Sprintf(`sources:
  - name: oauth-account
    type: imap
    host: mail.example.invalid
    port: 993
    username: fixture@example.invalid
    max_connections: 1
    oauth2:
      client_id: fixture-client
      token_url: http://oauth.example.invalid/token
targets:
  - name: local
    type: maildir
    config:
      path: %q
      create_if_missing: true
pairs:
  - source: oauth-account
    targets: [local]
`, targetPath)
	if err := os.WriteFile(cfgFile, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := captureOutput(t, func() error { return runValidateConfig(nil, nil) })
	if err == nil || !strings.Contains(err.Error(), "OAuth2 endpoints must use HTTPS") {
		t.Fatalf("expected an OAuth endpoint error, got %v", err)
	}
	var report validationReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("invalid validation JSON: %v", err)
	}
	if report.Valid || report.ConfigurationValid || report.ConnectivityTested {
		t.Errorf("expected structural validation failure before connectivity, got %s", output)
	}
	if _, err := os.Stat(targetPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("target path stat after invalid configuration: %v, want not exist", err)
	}
}

func TestValidateConfigResolvesUnusedResourceCredentials(t *testing.T) {
	configureCLI(t)
	jsonOutput = true
	t.Setenv("MAILVAULT_VALIDATION_UNUSED", "")
	data, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	values["targets"] = append(mustAssert[[]any](t, values["targets"]), map[string]any{
		"name": "unused", "type": "s3", "config": map[string]any{"bucket": "fixture", "region": "us-east-1", "credentials_ref": "env:MAILVAULT_VALIDATION_UNUSED"}, // #nosec G101 -- env credential reference, not a secret
	})
	data, err = json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := captureOutput(t, func() error { return runValidateConfig(nil, nil) })
	if err == nil || !strings.Contains(err.Error(), "MAILVAULT_VALIDATION_UNUSED") {
		t.Fatalf("validation did not resolve all declared resources: %v", err)
	}
	if !json.Valid([]byte(output)) {
		t.Fatalf("invalid error JSON: %s", output)
	}
}
