package main

import (
	"context"
	"errors"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins"
)

type loginProbeSource struct {
	token             func(context.Context) (string, error)
	connected, closed bool
	failure           error
}

func (*loginProbeSource) Name() string { return "hotmail" }
func (*loginProbeSource) Type() string { return "imap" }
func (src *loginProbeSource) Connect(ctx context.Context) error {
	src.connected = true
	token, err := src.token(ctx)
	if err != nil {
		return err
	}
	if token != "fresh-token" {
		return errors.New("did not use new login token")
	}
	return src.failure
}
func (src *loginProbeSource) Close() error { src.closed = true; return nil }
func (*loginProbeSource) ListFolders(context.Context) ([]string, error) {
	panic("login must not list folders")
}
func (*loginProbeSource) FetchMessages(context.Context, []string, int64, int64) (<-chan *plugins.Message, <-chan error) {
	panic("login must not fetch mail")
}

func TestMailboxTokenValidatorUsesFreshTokenAndClosesConnection(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "accepted"
		if reject {
			name = "rejected"
		}
		t.Run(name, func(t *testing.T) {
			failure := errors.New("authentication rejected")
			probe := &loginProbeSource{}
			if reject {
				probe.failure = failure
			}
			registry := plugins.NewRegistry()
			registry.RegisterSource("imap", func(values map[string]any) (plugins.Source, error) {
				if values["username"] != "expected@hotmail.com" || values["host"] != "outlook.office365.com" {
					t.Fatal("wrong mailbox configuration")
				}
				probe.token = mustAssert[func(context.Context) (string, error)](t, values["oauth2_token_provider"])
				return probe, nil
			})
			cfg := config.SourceConfig{Name: "hotmail", Type: "imap", Host: "outlook.office365.com", Username: "expected@hotmail.com", UseTLS: true, OAuth2: &config.OAuth2Config{ClientID: "client", Tenant: "consumers"}}
			err := mailboxTokenValidator(&cfg, registry)(t.Context(), "fresh-token")
			if reject && !errors.Is(err, failure) {
				t.Fatalf("lost authentication failure: %v", err)
			}
			if !reject && err != nil {
				t.Fatal(err)
			}
			if !probe.connected || !probe.closed {
				t.Fatal("connection was not checked and closed")
			}
		})
	}
}
