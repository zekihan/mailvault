package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadResolvesRelativeCredentialFilesAndIndexedEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte("file-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	data := "sources:\n" +
		"  - name: old\n    type: imap\n    host: old.invalid\n    port: 993\n    username: old@example.invalid\n    password_ref: file:secret\n    use_tls: true\n    max_connections: 1\n" +
		"targets:\n  - name: local\n    type: maildir\n    config: {}\n" +
		"pairs:\n  - source: old\n    targets: [local]\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILVAULT_PAIRS_0_SOURCE", "new")
	t.Setenv("MAILVAULT_SOURCES_0_NAME", "new")
	t.Setenv("MAILVAULT_SOURCES_0_HOST", "new.invalid")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources[0].Name != "new" || cfg.Sources[0].Host != "new.invalid" || cfg.Sources[0].PasswordRef != "file-value" {
		t.Fatalf("indexed environment or config-relative credential mismatch: %+v", cfg.Sources[0])
	}
}

func TestLoadOAuthSourceSkipsUnrelatedCredentialsAndRejectsClientSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "sources:\n" +
		"  - name: oauth\n    type: imap\n    host: outlook.office365.com\n    port: 993\n    username: user@example.invalid\n    oauth2:\n      client_id: public-client\n" +
		"  - name: unrelated\n    type: pop3\n    host: mail.example.invalid\n    port: 995\n    username: other@example.invalid\n    password_ref: env:UNSET_TEST_SECRET\n" +
		"targets: []\npairs: []\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOAuthSource(path, "oauth"); err != nil {
		t.Fatalf("selected OAuth source depended on unrelated credential: %v", err)
	}
	data = strings.Replace(data, "client_id: public-client", "client_id: public-client\n      client_secret: env:UNSUPPORTED_SECRET", 1)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOAuthSource(path, "oauth"); err == nil || !strings.Contains(err.Error(), "client_secret is not supported") {
		t.Fatalf("OAuth client secret was silently accepted: %v", err)
	}
}
