package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeviceLoginValidatesBeforeReplacingCredentials(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "accepted"
		if reject {
			name = "rejected"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/devicecode" {
					_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "device", "user_code": "code", "verification_uri": "https://example.invalid", "expires_in": 60, "interval": 1})
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600})
				}
			}))
			defer server.Close()
			store := memoryStore{"account": "original-credentials"}
			called := false
			failure := errors.New("mailbox rejected")
			cfg := Config{ClientID: "client", DeviceCodeURL: server.URL + "/devicecode", TokenURL: server.URL + "/token", HTTPClient: server.Client(), ValidateAccessToken: func(ctx context.Context, token string) error {
				called = true
				if token != "new-access" || store["account"] != "original-credentials" {
					t.Fatal("validation did not receive unsaved token")
				}
				if reject {
					return failure
				}
				return nil
			}}
			var output strings.Builder
			err := DeviceLogin(context.Background(), cfg, "account", store, &output)
			if !called {
				t.Fatal("validation skipped")
			}
			if reject {
				if !errors.Is(err, failure) || !strings.Contains(err.Error(), "new credentials were not saved") || store["account"] != "original-credentials" {
					t.Fatalf("failed validation replaced credentials or lost error: %v", err)
				}
			} else if err != nil || store["account"] == "original-credentials" {
				t.Fatalf("successful validation did not save credentials: %v", err)
			}
			if strings.Contains(output.String(), "new-access") || strings.Contains(output.String(), "new-refresh") {
				t.Fatal("tokens leaked into output")
			}
		})
	}
}
