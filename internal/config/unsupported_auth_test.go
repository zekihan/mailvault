package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsUnsupportedAuthentication(t *testing.T) {
	for _, kind := range []string{"imap", "pop3"} {
		t.Run(kind, func(t *testing.T) {
			cfg := &Config{Sources: []SourceConfig{{Name: "account", Type: kind, Host: "example.invalid", Port: 993, Username: "test-user", MaxConnections: 1, OAuth2: &OAuth2Config{ClientID: "test-client"}}}, Targets: []TargetConfig{{Name: "local", Type: "maildir"}}, Pairs: []PairConfig{{Source: "account", Targets: []string{"local"}}}}
			if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "OAuth2") {
				t.Fatalf("OAuth-only config accepted or wrong error: %v", err)
			}
			cfg.Sources[0].PasswordRef = "synthetic-value"
			if err := Validate(cfg); err != nil {
				t.Fatalf("password authentication rejected: %v", err)
			}
			if kind == "pop3" {
				cfg.Sources[0].StartTLS = true
				if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
					t.Fatalf("POP3 STARTTLS accepted: %v", err)
				}
			}
		})
	}
}
