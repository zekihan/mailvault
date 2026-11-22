package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileTokenProviderRejectsMissingAndCorruptState(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name, content, message string
		want                   error
	}{
		{name: "missing", message: "read OAuth state", want: os.ErrNotExist},
		{name: "corrupt envelope", content: "{", message: "OAuth token file is invalid"},
		{name: "corrupt token state", content: `{"account":"account","state":"{"}`, message: "OAuth state missing or invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := FileStore{Path: filepath.Join(t.TempDir(), "token.json")}
			if tc.content != "" {
				if err := os.WriteFile(store.Path, []byte(tc.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			provider, err := NewTokenProvider(loopbackOAuthConfig(server), "account", store)
			if err != nil {
				t.Fatal(err)
			}
			token, err := provider.AccessToken(t.Context())
			if token != "" || err == nil || !strings.Contains(err.Error(), tc.message) || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("token lookup error = %v, want %q", err, tc.message)
			}
			if tc.content == "" {
				if _, err := os.Stat(store.Path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("token lookup created a token file: %v", err)
				}
			} else if got, err := os.ReadFile(store.Path); err != nil || string(got) != tc.content {
				t.Fatalf("token lookup changed corrupt state: %v", err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("unusable state caused %d provider requests", requests.Load())
	}
}

func TestFileTokenProviderPersistsSuccessfulRefresh(t *testing.T) {
	const oldAccess, oldRefresh = "first-fixture-access", "first-fixture-refresh"
	const newAccess, newRefresh = "second-fixture-access", "second-fixture-refresh"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/token" || r.ParseForm() != nil ||
			r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != oldRefresh {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: newAccess, RefreshToken: newRefresh, ExpiresIn: 3600})
	}))
	defer server.Close()

	store := FileStore{Path: filepath.Join(t.TempDir(), "nested", "token.json")}
	save := func(expiry time.Time) {
		t.Helper()
		data, err := json.Marshal(tokenState{AccessToken: oldAccess, RefreshToken: oldRefresh, Expiry: expiry})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Set(t.Context(), "account", string(data)); err != nil {
			t.Fatal(err)
		}
	}
	open := func() *Provider {
		t.Helper()
		provider, err := NewTokenProvider(loopbackOAuthConfig(server), "account", store)
		if err != nil {
			t.Fatal(err)
		}
		return provider
	}
	provider := open()
	save(time.Now().Add(time.Hour))
	token, err := provider.AccessToken(t.Context())
	if token != oldAccess || err != nil || requests.Load() != 0 {
		t.Fatalf("valid cached token unexpectedly refreshed: %v", err)
	}

	save(time.Now().Add(-time.Hour))
	start := time.Now()
	token, err = provider.AccessToken(t.Context())
	if token != newAccess || err != nil || requests.Load() != 1 {
		t.Fatalf("expired token did not refresh exactly once: %v", err)
	}
	info, err := os.Stat(store.Path)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatalf("refreshed token file is not private: %v, %v", info, err)
	}
	saved, err := store.Get(t.Context(), "account")
	if err != nil {
		t.Fatal(err)
	}
	var state tokenState
	if err := json.Unmarshal([]byte(saved), &state); err != nil {
		t.Fatal(err)
	}
	if state.AccessToken != newAccess || state.RefreshToken != newRefresh ||
		state.Expiry.Before(start.Add(time.Hour)) || state.Expiry.After(time.Now().Add(time.Hour)) {
		t.Fatal("refreshed credentials or expiry were not persisted")
	}
	token, err = open().AccessToken(t.Context())
	if token != newAccess || err != nil || requests.Load() != 1 {
		t.Fatalf("new provider did not reuse refreshed file: %v", err)
	}
}

func TestDeviceLoginRejectsInvalidProviderResponses(t *testing.T) {
	const instructions = `{"device_code":"device-fixture","user_code":"CODE-TEST","verification_uri":"https://example.invalid","expires_in":60,"interval":1}`
	const responseDetail = "fixture-private-detail"
	for _, tc := range []struct {
		name, device, token, message string
		status, tokenRequests        int
	}{
		{name: "malformed device response", device: "{", message: "invalid device authorization response"},
		{name: "incomplete device response", device: "{}", message: "incomplete device authorization response"},
		{name: "unsafe verification link", device: strings.Replace(instructions, "https://", "http://", 1), message: "invalid device authorization instructions"},
		{name: "declined", device: instructions, token: `{"error":"authorization_declined","error_description":"` + responseDetail + `"}`, message: "authorization_declined", status: http.StatusBadRequest, tokenRequests: 1},
		{name: "missing refresh token", device: instructions, token: `{"access_token":"` + responseDetail + `","expires_in":3600}`, message: "did not include a refresh token", tokenRequests: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var tokenRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/devicecode":
					_, _ = fmt.Fprint(w, tc.device)
				case "/token":
					tokenRequests.Add(1)
					if tc.status != 0 {
						w.WriteHeader(tc.status)
					}
					_, _ = fmt.Fprint(w, tc.token)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			var output bytes.Buffer
			store := FileStore{Path: filepath.Join(t.TempDir(), "token.json")}
			err := DeviceLogin(t.Context(), loopbackOAuthConfig(server), "account", store, &output)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("expected login failure mentioning %q", tc.message)
			}
			if strings.Contains(err.Error(), responseDetail) || strings.Contains(output.String(), responseDetail) {
				t.Fatal("login disclosed private provider response data")
			}
			if got := tokenRequests.Load(); got != int32(tc.tokenRequests) {
				t.Fatalf("sent %d token requests, want %d", got, tc.tokenRequests)
			}
			if _, err := os.Stat(store.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed login created a token file: %v", err)
			}
		})
	}
}

func loopbackOAuthConfig(server *httptest.Server) Config {
	client := server.Client()
	client.Timeout = 3 * time.Second
	return Config{
		ClientID: "fixture-client", DeviceCodeURL: server.URL + "/devicecode",
		TokenURL: server.URL + "/token", HTTPClient: client,
	}
}
