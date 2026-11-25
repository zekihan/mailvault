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

func newLoginValidationServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/devicecode" {
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "device", "user_code": "code", "verification_uri": "https://example.invalid", "expires_in": 60, "interval": 1})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600})
		}
	}))
}

type loginValidationFixture struct {
	server  *httptest.Server
	store   memoryStore
	called  bool
	failure error
}

func (fixture *loginValidationFixture) open(t *testing.T, reject bool) Config {
	t.Helper()
	fixture.store = memoryStore{"account": "original-credentials"}
	fixture.failure = errors.New("mailbox rejected")
	return Config{ClientID: "client", DeviceCodeURL: fixture.server.URL + "/devicecode", TokenURL: fixture.server.URL + "/token", HTTPClient: fixture.server.Client(), ValidateAccessToken: func(_ context.Context, token string) error {
		return fixture.validate(t, reject, token)
	}}
}

func (fixture *loginValidationFixture) validate(t *testing.T, reject bool, token string) error {
	t.Helper()
	fixture.called = true
	if token != "new-access" || fixture.store["account"] != "original-credentials" {
		t.Fatal("validation did not receive unsaved token")
	}
	if reject {
		return fixture.failure
	}
	return nil
}

func (fixture *loginValidationFixture) checkOutcome(t *testing.T, err error, reject bool) {
	t.Helper()
	if !fixture.called {
		t.Fatal("validation skipped")
	}
	if reject {
		fixture.checkRejected(t, err)
	} else {
		fixture.checkAccepted(t, err)
	}
}

func (fixture *loginValidationFixture) checkRejected(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, fixture.failure) || !strings.Contains(err.Error(), "new credentials were not saved") || fixture.store["account"] != "original-credentials" {
		t.Fatalf("failed validation replaced credentials or lost error: %v", err)
	}
}

func (fixture *loginValidationFixture) checkAccepted(t *testing.T, err error) {
	t.Helper()
	if err != nil || fixture.store["account"] == "original-credentials" {
		t.Fatalf("successful validation did not save credentials: %v", err)
	}
}

func TestDeviceLoginValidatesBeforeReplacingCredentials(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "accepted"
		if reject {
			name = "rejected"
		}
		t.Run(name, func(t *testing.T) {
			fixture := &loginValidationFixture{server: newLoginValidationServer()}
			defer fixture.server.Close()
			cfg := fixture.open(t, reject)
			var output strings.Builder
			err := DeviceLogin(t.Context(), &cfg, "account", fixture.store, &output)
			fixture.checkOutcome(t, err, reject)
			if strings.Contains(output.String(), "new-access") || strings.Contains(output.String(), "new-refresh") {
				t.Fatal("tokens leaked into output")
			}
		})
	}
}
