package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/testutil/mailtest"
)

func TestValidateAndListConfigurationText(t *testing.T) {
	_, port := mailtest.POP3Server(t, nil, nil)
	configureCLIOnPort(t, port, "3s")
	jsonOutput = false
	t.Setenv("MAILVAULT_SOURCES_0_FOLDERS", "INBOX")
	for _, tc := range []struct {
		name string
		run  func() error
		want []string
	}{
		{"list-sources", func() error { return runListSources(nil, nil) }, []string{"test-account (pop3://127.0.0.1:", "Username: test-user", "Folders: INBOX"}},
		{"list-targets", func() error { return runListTargets(nil, nil) }, []string{"local (maildir)", "credentials_ref: [REDACTED]"}},
		{"validate-config", func() error { return runValidateConfig(nil, nil) }, []string{"Configuration is valid", "test-account -> local", "Source test-account: OK", "Target local: OK"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := captureOutput(t, tc.run)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output, "synthetic-sensitive-value") {
				t.Fatal("command output disclosed a credential")
			}
			for _, want := range tc.want {
				if !strings.Contains(output, want) {
					t.Fatalf("missing %q in %q", want, output)
				}
			}
		})
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("configuration commands left state entries %v, error = %v", entries, err)
	}
}

func TestValidateConfigReportsInvalidTargetOptions(t *testing.T) {
	_, port := mailtest.POP3Server(t, nil, nil)
	configureCLIOnPort(t, port, "3s")
	jsonOutput = true
	for _, tc := range []struct {
		targetType, message string
	}{
		{"maildir", "path is required"},
		{"mbox", "path is required"},
		{"s3", "bucket is required"},
	} {
		t.Run(tc.targetType, func(t *testing.T) {
			t.Setenv("MAILVAULT_TARGETS_0_TYPE", tc.targetType)
			t.Setenv("MAILVAULT_TARGETS_0_CONFIG_PATH", "")
			t.Setenv("MAILVAULT_TARGETS_0_CONFIG_CREDENTIALS_REF", `{"access_key_id":"test-access","secret_access_key":"test-secret"}`)
			output, err := captureOutput(t, func() error { return runValidateConfig(nil, nil) })
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("validate-config error = %v, want %q", err, tc.message)
			}
			var report validationReport
			if err := json.Unmarshal([]byte(output), &report); err != nil {
				t.Fatal(err)
			}
			want := validationResource{Name: "local", Type: tc.targetType, Status: "failed", Error: tc.message}
			if report.Valid || !report.ConfigurationValid || !report.ConnectivityTested || len(report.Targets) != 1 || report.Targets[0] != want {
				t.Fatalf("validation report = %+v, want target %+v", report, want)
			}
			if _, err := os.Stat(filepath.Join(stateDir, "state.db")); !os.IsNotExist(err) {
				t.Fatalf("validate-config wrote state: %v", err)
			}
		})
	}
}
