package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func credentialConfigFile(t *testing.T, sourceRef, targetRef string) string {
	t.Helper()
	values := map[string]interface{}{
		"sources": []interface{}{map[string]interface{}{"name": "active", "password_ref": "synthetic-value"}, map[string]interface{}{"name": "unused-source", "password_ref": sourceRef}},
		"targets": []interface{}{map[string]interface{}{"name": "local", "config": map[string]interface{}{}}, map[string]interface{}{"name": "unused-target", "config": map[string]interface{}{"credentials_ref": targetRef}}},
		"pairs":   []interface{}{map[string]interface{}{"source": "active", "targets": []string{"local"}}},
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDoesNotResolveUnusedCredentials(t *testing.T) {
	t.Setenv("MAILVAULT_UNUSED_FIXTURE", "")
	for _, reference := range []string{"env:MAILVAULT_UNUSED_FIXTURE", "file:" + filepath.Join(t.TempDir(), "missing")} {
		path := credentialConfigFile(t, reference, reference)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unused credentials blocked active config: %v", err)
		}
		if cfg.Sources[1].PasswordRef != reference || cfg.Targets[1].Config["credentials_ref"] != reference {
			t.Fatal("unused credentials were changed")
		}
	}
}

func TestLoadSelectedDoesNotResolveUnselectedCredentials(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-secret")
	path := credentialConfigFile(t, "file:"+missing, "file:"+missing)
	cfg, err := LoadSelected(path, "", "local")
	if err != nil {
		t.Fatalf("selected target load resolved unrelated source or target credentials: %v", err)
	}
	if cfg.Sources[1].PasswordRef != "file:"+missing || cfg.Targets[1].Config["credentials_ref"] != "file:"+missing {
		t.Fatal("unselected credential references changed")
	}
}

func TestLoadRejectsMissingActiveCredentials(t *testing.T) {
	t.Setenv("MAILVAULT_ACTIVE_FIXTURE", "")
	path := credentialConfigFile(t, "env:MAILVAULT_ACTIVE_FIXTURE", "env:MAILVAULT_ACTIVE_FIXTURE")
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"source", "target"} {
		var values map[string]interface{}
		if err := json.Unmarshal(encoded, &values); err != nil {
			t.Fatal(err)
		}
		pair := values["pairs"].([]interface{})[0].(map[string]interface{})
		if resource == "source" {
			pair["source"] = "unused-source"
		} else {
			pair["targets"] = []string{"unused-target"}
		}
		data, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("missing active %s credential accepted", resource)
		}
	}
}

func TestSourcePasswordFilesNormalizeOnlyLineTerminator(t *testing.T) {
	for _, tc := range []struct {
		name, data, want string
		invalid          bool
	}{
		{name: "LF", data: "synthetic-value\n", want: "synthetic-value"},
		{name: "CRLF", data: "synthetic-value\r\n", want: "synthetic-value"},
		{name: "spaces", data: " synthetic-value \n", want: " synthetic-value "},
		{name: "plain", data: "synthetic-value", want: "synthetic-value"},
		{name: "multiline", data: "synthetic-value\nNOOP\n", invalid: true},
		{name: "extra-newline", data: "synthetic-value\n\n", invalid: true},
		{name: "embedded-CR", data: "synthetic\rvalue", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secretPath := filepath.Join(t.TempDir(), "fixture-value")
			if err := os.WriteFile(secretPath, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			path := credentialConfigFile(t, "file:"+secretPath, "")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var values map[string]interface{}
			if err := json.Unmarshal(raw, &values); err != nil {
				t.Fatal(err)
			}
			values["pairs"].([]interface{})[0].(map[string]interface{})["source"] = "unused-source"
			encoded, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tc.invalid {
				if err == nil {
					t.Fatal("multiline source password accepted")
				}
				if strings.Contains(err.Error(), "synthetic") {
					t.Fatal("error exposed credential")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Sources[1].PasswordRef != tc.want {
				t.Fatal("source password bytes not normalized as expected")
			}
		})
	}
}

func TestSourceEnvironmentAndLiteralPasswordsRejectNewlines(t *testing.T) {
	t.Setenv("MAILVAULT_LINEBREAK_FIXTURE", "synthetic-value\r\nNOOP")
	for _, reference := range []string{"env:MAILVAULT_LINEBREAK_FIXTURE", "synthetic-value\nNOOP"} {
		cfg := &Config{Sources: []SourceConfig{{Name: "active", PasswordRef: reference}}}
		if err := expandCredentialRefs(cfg); err == nil {
			t.Fatal("source credential line break accepted")
		}
	}
}

func TestTargetCredentialFilesKeepMultilineJSON(t *testing.T) {
	data := "{\n  \"access_key_id\": \"synthetic-id\",\n  \"secret_access_key\": \"synthetic-key\"\n}\n"
	path := filepath.Join(t.TempDir(), "fixture-json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Targets: []TargetConfig{{Name: "s3", Config: map[string]interface{}{"credentials_ref": "file:" + path}}}}
	if err := expandCredentialRefs(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Targets[0].Config["credentials_ref"] != data {
		t.Fatal("multiline target credentials were changed")
	}
}
