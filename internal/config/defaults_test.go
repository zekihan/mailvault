package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSourceDefaultsUseDecodedConfigurationPresence(t *testing.T) {
	// AutomaticEnv does not decode indexed source values. Such variables must
	// not suppress defaults by claiming an omitted file field is present.
	t.Setenv("MAILVAULT_SOURCES.0.USE_TLS", "true")
	t.Setenv("MAILVAULT_SOURCES.0.CONNECTION_TIMEOUT", "30s")
	t.Setenv("MAILVAULT_SOURCES.0.READ_TIMEOUT", "60s")
	t.Run("file-config", TestLoadSourceDefaultsPreservesExplicitValues)
}

func TestLoadSourceDefaultsPreservesExplicitValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`sources:
  - name: secure-defaults
    type: pop3
    host: example.invalid
    port: 995
    username: test-user
    password_ref: synthetic-value
    max_connections: 1
  - name: explicit-values
    type: pop3
    host: example.invalid
    port: 110
    username: test-user
    password_ref: synthetic-value
    use_tls: false
    connection_timeout: 0s
    read_timeout: 0s
    max_connections: 1
  - name: starttls
    type: imap
    host: example.invalid
    port: 143
    username: test-user
    password_ref: synthetic-value
    start_tls: true
    max_connections: 1
  - name: custom-timeouts
    type: imap
    host: example.invalid
    port: 993
    username: test-user
    password_ref: synthetic-value
    use_tls: true
    connection_timeout: 7s
    read_timeout: 11s
    max_connections: 1
`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	defaults := cfg.Sources[0]
	if !defaults.UseTLS || defaults.ConnectionTimeout != 30*time.Second || defaults.ReadTimeout != 60*time.Second {
		t.Fatalf("unsafe or missing defaults: TLS=%v dial=%v read=%v", defaults.UseTLS, defaults.ConnectionTimeout, defaults.ReadTimeout)
	}
	explicit := cfg.Sources[1]
	if explicit.UseTLS || explicit.ConnectionTimeout != 0 || explicit.ReadTimeout != 0 {
		t.Fatal("explicit false/zero settings were overridden")
	}
	starttls := cfg.Sources[2]
	if starttls.UseTLS || !starttls.StartTLS || starttls.ConnectionTimeout != 30*time.Second || starttls.ReadTimeout != 60*time.Second {
		t.Fatal("STARTTLS defaults selected implicit TLS or lost timeouts")
	}
	custom := cfg.Sources[3]
	if !custom.UseTLS || custom.ConnectionTimeout != 7*time.Second || custom.ReadTimeout != 11*time.Second {
		t.Fatal("explicit TLS/timeouts were changed")
	}
}
