package source

import (
	"context"
	"strings"
	"testing"
)

func TestPOP3AcceptsSTARTTLS(t *testing.T) {
	source, err := NewPOP3Source(map[string]interface{}{"host": "example.invalid", "username": "test-user", "password_ref": "synthetic-value", "use_tls": false, "start_tls": true})
	if err != nil {
		t.Fatalf("STARTTLS rejected: %v", err)
	}
	_ = source.Close()
}

func TestPOP3RejectsConflictingTLSModes(t *testing.T) {
	_, err := NewPOP3Source(map[string]interface{}{"host": "example.invalid", "username": "test-user", "password_ref": "synthetic-value", "use_tls": true, "start_tls": true})
	if err == nil {
		t.Fatal("conflicting TLS modes accepted")
	}
}

func TestSourcesRejectOAuthWithoutTLS(t *testing.T) {
	provider := func(context.Context) (string, error) { return "synthetic-token", nil }
	for _, tc := range []struct {
		name string
		new  func() error
	}{
		{"imap", func() error {
			_, err := NewIMapSource(map[string]interface{}{"host": "example.invalid", "username": "user", "oauth2_token_provider": provider, "use_tls": false, "start_tls": false})
			return err
		}},
		{"pop3", func() error {
			_, err := NewPOP3Source(map[string]interface{}{"host": "example.invalid", "username": "user", "oauth2_token_provider": provider, "use_tls": false, "start_tls": false})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.new(); err == nil || !strings.Contains(err.Error(), "requires TLS") {
				t.Fatalf("cleartext OAuth accepted: %v", err)
			}
		})
	}
}
