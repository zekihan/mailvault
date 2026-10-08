package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidate_Success(t *testing.T) {
	cfg := &Config{
		Sources: []SourceConfig{
			{
				Name:        "gmail",
				Type:        "imap",
				Host:        "imap.gmail.com",
				Port:        993,
				Username:    "user@gmail.com",
				PasswordRef: "env:GMAIL_PASS",
				UseTLS:      true,
				MaxConnections: 5,
			},
		},
		Targets: []TargetConfig{
			{
				Name: "local",
				Type: "maildir",
				Config: map[string]interface{}{
					"path": "/tmp/mail",
				},
			},
		},
		Pairs: []PairConfig{
			{Source: "gmail", Targets: []string{"local"}},
		},
	}

	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidate_MissingSource(t *testing.T) {
	cfg := &Config{
		Sources: []SourceConfig{},
		Targets: []TargetConfig{
			{Name: "local", Type: "maildir", Config: map[string]interface{}{"path": "/tmp"}},
		},
		Pairs: []PairConfig{{Source: "gmail", Targets: []string{"local"}}},
	}

	if err := Validate(cfg); err == nil {
		t.Fatal("Validate() = nil, want error")
	}
}

func TestValidate_DuplicateSourceName(t *testing.T) {
	cfg := &Config{
		Sources: []SourceConfig{
			{Name: "a", Type: "imap", Host: "h", Port: 993, Username: "u", PasswordRef: "p", MaxConnections: 1},
			{Name: "a", Type: "imap", Host: "h", Port: 993, Username: "u", PasswordRef: "p", MaxConnections: 1},
		},
		Targets: []TargetConfig{{Name: "local", Type: "maildir", Config: map[string]interface{}{"path": "/tmp"}}},
		Pairs:   []PairConfig{{Source: "a", Targets: []string{"local"}}},
	}

	if err := Validate(cfg); err == nil {
		t.Fatal("Validate() = nil, want error for duplicate name")
	}
}

func TestValidate_UnknownSourceType(t *testing.T) {
	cfg := &Config{
		Sources: []SourceConfig{
			{Name: "a", Type: "jmap", Host: "h", Port: 993, Username: "u", PasswordRef: "p", MaxConnections: 1},
		},
		Targets: []TargetConfig{{Name: "local", Type: "maildir", Config: map[string]interface{}{"path": "/tmp"}}},
		Pairs:   []PairConfig{{Source: "a", Targets: []string{"local"}}},
	}

	if err := Validate(cfg); err == nil {
		t.Fatal("Validate() = nil, want error for unknown type")
	}
}

func TestResolveCredentialRef_Env(t *testing.T) {
	os.Setenv("TEST_MAIL_PASS", "secret123")
	defer os.Unsetenv("TEST_MAIL_PASS")

	val, err := resolveCredentialRef("env:TEST_MAIL_PASS")
	if err != nil {
		t.Fatalf("resolveCredentialRef() = %v, want nil", err)
	}
	if val != "secret123" {
		t.Fatalf("resolveCredentialRef() = %q, want %q", val, "secret123")
	}
}

func TestResolveCredentialRef_File(t *testing.T) {
	tmpDir := t.TempDir()
	secretFile := filepath.Join(tmpDir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("file-secret\n"), 0600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	val, err := resolveCredentialRef("file:" + secretFile)
	if err != nil {
		t.Fatalf("resolveCredentialRef() = %v, want nil", err)
	}
	if val != "file-secret\n" {
		t.Fatalf("resolveCredentialRef() = %q, want %q", val, "file-secret\n")
	}
}

func TestResolveCredentialRef_Plain(t *testing.T) {
	val, err := resolveCredentialRef("plain-password")
	if err != nil {
		t.Fatalf("resolveCredentialRef() = %v, want nil", err)
	}
	if val != "plain-password" {
		t.Fatalf("resolveCredentialRef() = %q, want %q", val, "plain-password")
	}
}

func TestLoad_FromFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configYAML := `
sources:
  - name: test
    type: imap
    host: imap.example.com
    port: 993
    username: user
    password_ref: "plain-pass"
    use_tls: true
    max_connections: 5
targets:
  - name: local
    type: maildir
    config:
      path: /tmp/mail
pairs:
  - source: test
    targets: [local]
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0].Name != "test" {
		t.Fatalf("Load() sources = %+v, want 1 source named 'test'", cfg.Sources)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0].Name != "local" {
		t.Fatalf("Load() targets = %+v, want 1 target named 'local'", cfg.Targets)
	}
	if len(cfg.Pairs) != 1 || cfg.Pairs[0].Source != "test" {
		t.Fatalf("Load() pairs = %+v, want 1 pair", cfg.Pairs)
	}
}