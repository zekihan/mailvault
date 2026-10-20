package config

import (
	"strings"
	"testing"
)

func TestValidateAcceptsOAuthAndRejectsAmbiguousAuthentication(t *testing.T) {
	for _, kind := range []string{"imap", "pop3"} {
		t.Run(kind, func(t *testing.T) {
			cfg := &Config{Sources: []SourceConfig{{Name: "account", Type: kind, Host: "example.invalid", Port: 993, Username: "test-user", UseTLS: true, MaxConnections: 1, OAuth2: &OAuth2Config{ClientID: "test-client"}}}, Targets: []TargetConfig{{Name: "local", Type: "maildir"}}, Pairs: []PairConfig{{Source: "account", Targets: []string{"local"}}}}
			if err := Validate(cfg); err != nil {
				t.Fatalf("OAuth-only config rejected: %v", err)
			}
			cfg.Sources[0].PasswordRef = "synthetic-value"
			if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "either password_ref or oauth2") {
				t.Fatalf("ambiguous authentication accepted: %v", err)
			}
			cfg.Sources[0].OAuth2 = nil
			if err := Validate(cfg); err != nil {
				t.Fatalf("password authentication rejected: %v", err)
			}
		})
	}
}
