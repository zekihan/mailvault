package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type failingResponseReader func() error

func (f failingResponseReader) Read([]byte) (int, error) { return 0, f() }

type interruptedResponseTransport struct {
	readError failingResponseReader
}

func (tr interruptedResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	_ = req.Body.Close()
	body := io.MultiReader(strings.NewReader("{"), tr.readError)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body)}, nil
}

func TestOAuthResponseReadPreservesContextErrors(t *testing.T) {
	requests := []struct {
		name    string
		request func(context.Context, Config) error
	}{
		{"device authorization", func(ctx context.Context, cfg Config) error {
			_, err := requestDeviceCode(ctx, &cfg)
			return err
		}},
		{"token exchange", func(ctx context.Context, cfg Config) error {
			_, err := requestToken(ctx, &cfg, url.Values{"grant_type": {"refresh_token"}})
			return err
		}},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
				t.Run(want.Error(), func(t *testing.T) {
					ctx, cancel := context.WithCancelCause(t.Context())
					defer cancel(nil)
					transport := interruptedResponseTransport{readError: func() error {
						if errors.Is(want, context.Canceled) {
							cause := errors.New("synthetic interrupt")
							cancel(cause)
							return cause
						}
						return context.DeadlineExceeded
					}}
					rawCfg := Config{ // #nosec G101 -- synthetic public client ID
						ClientID:      "synthetic-client",
						DeviceCodeURL: "https://example.invalid/devicecode",
						TokenURL:      "https://example.invalid/token",
						HTTPClient:    &http.Client{Transport: transport},
					}
					cfg := rawCfg.normalized()
					if err := request.request(ctx, cfg); !errors.Is(err, want) {
						t.Fatalf("request error = %v, want %v", err, want)
					}
				})
			}
		})
	}
}
