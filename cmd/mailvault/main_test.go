package main

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
)

func captureOutput(t *testing.T, run func() error) (string, error) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = previous; _ = file.Close() }()
	runErr := run()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), runErr
}

func configureCLI(t *testing.T) {
	t.Helper()
	oldFile, oldState, oldJSON := cfgFile, stateDir, jsonOutput
	oldDry, oldConcurrency, oldRetries := dryRun, concurrency, maxRetries
	oldDelay, oldSince, oldUntil := retryDelay, since, until
	t.Cleanup(func() {
		cfgFile, stateDir, jsonOutput = oldFile, oldState, oldJSON
		dryRun, concurrency, maxRetries = oldDry, oldConcurrency, oldRetries
		retryDelay, since, until = oldDelay, oldSince, oldUntil
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILVAULT_TEST_VALUE", "synthetic-sensitive-value")
	data := map[string]interface{}{
		"sources": []interface{}{map[string]interface{}{
			"name": "test-account", "type": "pop3", "host": "127.0.0.1", "port": port,
			"username": "test-user", "password_ref": "env:MAILVAULT_TEST_VALUE", "use_tls": false,
			"max_connections": 1, "connection_timeout": "200ms", "read_timeout": "200ms",
		}},
		"targets": []interface{}{map[string]interface{}{
			"name": "local", "type": "maildir", "config": map[string]interface{}{
				"path": filepath.Join(t.TempDir(), "mail"), "credentials_ref": "env:MAILVAULT_TEST_VALUE",
			},
		}},
		"pairs": []interface{}{map[string]interface{}{"source": "test-account", "targets": []string{"local"}}},
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	cfgFile = filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	stateDir, dryRun, concurrency, maxRetries = t.TempDir(), false, 1, 0
	retryDelay, since, until = "0s", "", ""
}

func TestListingsRedactResolvedCredentials(t *testing.T) {
	for _, mode := range []string{"sources-json", "targets-json", "targets-text"} {
		t.Run(mode, func(t *testing.T) {
			configureCLI(t)
			jsonOutput = mode != "targets-text"
			output, err := captureOutput(t, func() error {
				if mode == "sources-json" {
					return runListSources(nil, nil)
				}
				return runListTargets(nil, nil)
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{"synthetic-sensitive-value", "synthetic-oauth-value"} {
				if strings.Contains(output, value) {
					t.Fatal("listing exposed a resolved credential")
				}
			}
			if mode != "sources-json" && !strings.Contains(output, "[REDACTED]") {
				t.Fatalf("missing redaction marker: %s", output)
			}
			if jsonOutput && !json.Valid([]byte(output)) {
				t.Fatalf("invalid JSON: %s", output)
			}
		})
	}
}

func TestListingLeavesOperationalCredentialsIntact(t *testing.T) {
	configureCLI(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Sources[0].OAuth2 = &config.OAuth2Config{ClientID: "visible-client", ClientSecret: "synthetic-oauth-value"}
	public := listingConfig(cfg)
	public.Targets[0].Config["path"] = "changed"
	if cfg.Sources[0].PasswordRef != "synthetic-sensitive-value" || cfg.Sources[0].OAuth2.ClientSecret != "synthetic-oauth-value" || cfg.Targets[0].Config["credentials_ref"] != "synthetic-sensitive-value" || cfg.Targets[0].Config["path"] == "changed" {
		t.Fatal("redaction mutated operational configuration")
	}
}

func TestSyncReturnsFailureAfterPrintingStats(t *testing.T) {
	configureCLI(t)
	jsonOutput = false
	output, err := captureOutput(t, func() error { return runSync(nil, nil) })
	if err == nil {
		t.Fatal("sync returned success for a refused source connection")
	}
	if !strings.Contains(output, "ERROR") || !strings.Contains(output, "test-account") {
		t.Fatalf("missing pair diagnostics: %s", output)
	}
}

func TestValidateReturnsConnectivityFailure(t *testing.T) {
	configureCLI(t)
	_, err := captureOutput(t, func() error { return runValidateConfig(nil, nil) })
	if err == nil {
		t.Fatal("validate-config returned success for a refused source connection")
	}
}
