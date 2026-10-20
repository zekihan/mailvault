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

func TestLoadOAuthSourceRejectsInvalidSelectedSource(t *testing.T) {
	valid := "sources:\n  - name: selected\n    type: imap\n    host: outlook.office365.com\n    port: 993\n    username: user@example.invalid\n    oauth2:\n      client_id: public-client\n"
	for _, test := range []struct {
		name, replace, want string
	}{
		{"missing host", "    host: outlook.office365.com\n", "host is required"},
		{"missing username", "    username: user@example.invalid\n", "username is required"},
		{"mixed authentication", "    username: user@example.invalid\n", "either password_ref or oauth2"},
		{"no TLS", "    oauth2:\n", "OAuth2 requires TLS"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := valid
			switch test.name {
			case "no TLS":
				data = strings.Replace(data, test.replace, "    use_tls: false\n"+test.replace, 1)
			case "mixed authentication":
				data = strings.Replace(data, test.replace, test.replace+"    password_ref: env:UNUSED\n", 1)
			default:
				data = strings.Replace(data, test.replace, "", 1)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOAuthSource(path, "selected"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid selected source accepted or wrong error: %v", err)
			}
		})
	}
}

func TestIndexedEnvironmentPreservesExplicitFalseAndZero(t *testing.T) {
	path := writeEnvironmentConfig(t)
	t.Setenv("MAILVAULT_SOURCES_0_USE_TLS", "false")
	t.Setenv("MAILVAULT_SOURCES_0_CONNECTION_TIMEOUT", "0s")
	t.Setenv("MAILVAULT_SOURCES_0_READ_TIMEOUT", "0s")
	t.Setenv("MAILVAULT_SOURCES_0_MAX_CONNECTIONS", "0")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	source := cfg.Sources[0]
	if source.UseTLS || source.ConnectionTimeout != 0 || source.ReadTimeout != 0 || source.MaxConnections != 0 {
		t.Fatalf("explicit indexed environment values were defaulted: %+v", source)
	}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "max_connections") {
		t.Fatalf("invalid explicit zero max_connections did not fail validation: %v", err)
	}
}

func TestIndexedEnvironmentRejectsMalformedTypedValues(t *testing.T) {
	for _, test := range []struct{ key, value string }{
		{"USE_TLS", "perhaps"}, {"PORT", "nine"}, {"CONNECTION_TIMEOUT", "soon"}, {"MAX_CONNECTIONS", "many"},
	} {
		t.Run(test.key, func(t *testing.T) {
			t.Setenv("MAILVAULT_SOURCES_0_"+test.key, test.value)
			if _, err := Load(writeEnvironmentConfig(t)); err == nil {
				t.Fatalf("malformed %s override was accepted", test.key)
			}
		})
	}
}

func TestKeyringReferenceUsesInjectedLookup(t *testing.T) {
	var gotService, gotAccount string
	value, err := resolveCredentialRefAtWithKeyring("keyring:mailvault:account:one", t.TempDir(), func(service, account string) (string, error) {
		gotService, gotAccount = service, account
		return "synthetic-secret", nil
	})
	if err != nil || value != "synthetic-secret" || gotService != "mailvault" || gotAccount != "account:one" {
		t.Fatalf("injected lookup returned value=%q service=%q account=%q err=%v", value, gotService, gotAccount, err)
	}
}

func TestNewOAuthTokenProviderRejectsClientSecret(t *testing.T) {
	_, err := NewOAuthTokenProvider(SourceConfig{Name: "account", UseTLS: true, OAuth2: &OAuth2Config{ClientID: "public-client", ClientSecret: "unused-secret"}})
	if err == nil || !strings.Contains(err.Error(), "client_secret is not supported") {
		t.Fatalf("provider silently accepted an OAuth client secret: %v", err)
	}
}

func TestNewOAuthTokenProviderRequiresTLS(t *testing.T) {
	_, err := NewOAuthTokenProvider(SourceConfig{Name: "account", OAuth2: &OAuth2Config{ClientID: "public-client"}})
	if err == nil || !strings.Contains(err.Error(), "OAuth2 requires TLS") {
		t.Fatalf("provider accepted an unencrypted OAuth source: %v", err)
	}
}

func TestOAuthTokenAccountIsolatesClientAndTenant(t *testing.T) {
	source := SourceConfig{Host: "mail.example.invalid", Username: "user@example.invalid", OAuth2: &OAuth2Config{ClientID: "client-one", Tenant: "consumers"}}
	base := OAuthTokenAccount(source)
	if base != OAuthTokenAccount(source) {
		t.Fatal("same mailbox did not receive a stable account key")
	}
	changedClient := source
	changedClient.OAuth2 = &OAuth2Config{ClientID: "client-two", Tenant: "consumers"}
	changedTenant := source
	changedTenant.OAuth2 = &OAuth2Config{ClientID: "client-one", Tenant: "organizations"}
	if base == OAuthTokenAccount(changedClient) || base == OAuthTokenAccount(changedTenant) {
		t.Fatal("different client or tenant reused a keyring account")
	}
	source.OAuth2.TokenKey = "explicit-account"
	if got := OAuthTokenAccount(source); got != "explicit-account" {
		t.Fatalf("explicit token key = %q", got)
	}
}

func writeEnvironmentConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "sources:\n" +
		"  - name: account\n    type: imap\n    host: mail.example.invalid\n    port: 993\n    username: user@example.invalid\n    password_ref: synthetic-password\n    max_connections: 1\n" +
		"targets:\n  - name: local\n    type: maildir\n    config: {}\n" +
		"pairs:\n  - source: account\n    targets: [local]\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
