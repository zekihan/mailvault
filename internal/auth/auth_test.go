package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type memoryStore map[string]string

func (s memoryStore) Get(_ context.Context, key string) (string, error) { return s[key], nil }
func (s memoryStore) Set(_ context.Context, key, value string) error    { s[key] = value; return nil }

type trackingStore struct{ gets int }

func (s *trackingStore) Get(context.Context, string) (string, error) { s.gets++; return "", nil }
func (*trackingStore) Set(context.Context, string, string) error     { return nil }

func TestXOAUTH2WirePayload(t *testing.T) {
	_, response, err := NewXOAUTH2("user@example.com", "synthetic-token").Start()
	if err != nil {
		t.Fatal(err)
	}
	const want = "user=user@example.com\x01auth=Bearer synthetic-token\x01\x01"
	if string(response) != want {
		t.Fatalf("payload = %q, want %q", response, want)
	}
	if got := base64.StdEncoding.EncodeToString(response); got != "dXNlcj11c2VyQGV4YW1wbGUuY29tAWF1dGg9QmVhcmVyIHN5bnRoZXRpYy10b2tlbgEB" {
		t.Fatalf("wire payload = %q", got)
	}
	continuation, err := NewXOAUTH2("user@example.com", "synthetic-token").Next([]byte(`{"status":"401"}`))
	if err != nil || continuation == nil || len(continuation) != 0 {
		t.Fatalf("challenge response = %v, %v; want empty SASL response", continuation, err)
	}
}

func TestDeviceLoginAndRefresh(t *testing.T) {
	var tokenRequests, devicePolls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/devicecode") {
			if r.Form.Get("scope") == "" || r.Form.Get("client_id") != "synthetic-client" {
				t.Errorf("device form missing required fields: %v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "synthetic-device-code", "user_code": "ABCD-EFGH", "verification_uri": "https://example.invalid/device", "expires_in": 60, "interval": 1})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/token") {
			tokenRequests++
			switch r.Form.Get("grant_type") {
			case "urn:ietf:params:oauth:grant-type:device_code":
				devicePolls++
				if devicePolls == 1 {
					_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-initial-access", "refresh_token": "synthetic-refresh", "expires_in": 1})
				}
			case "refresh_token":
				if r.Form.Get("refresh_token") != "synthetic-refresh" {
					t.Errorf("refresh token = %q", r.Form.Get("refresh_token"))
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-access", "refresh_token": "synthetic-rotated", "expires_in": 3600})
			default:
				t.Errorf("unexpected grant type %q", r.Form.Get("grant_type"))
			}
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	store := memoryStore{}
	cfg := Config{ClientID: "synthetic-client", DeviceCodeURL: server.URL + "/devicecode", TokenURL: server.URL + "/token", HTTPClient: server.Client()}
	var instructions strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := DeviceLogin(ctx, cfg, "account-key", store, &instructions); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(instructions.String(), "ABCD-EFGH") || strings.Contains(instructions.String(), "synthetic-access") {
		t.Fatalf("unexpected device instructions: %q", instructions.String())
	}
	var saved tokenState
	if err := json.Unmarshal([]byte(store["account-key"]), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.RefreshToken != "synthetic-refresh" {
		t.Fatalf("saved refresh token = %q", saved.RefreshToken)
	}
	saved.Expiry = time.Now().Add(-time.Minute)
	encoded, _ := json.Marshal(saved)
	store["account-key"] = string(encoded)
	provider, err := NewTokenProvider(cfg, "account-key", store)
	if err != nil {
		t.Fatal(err)
	}
	got, err := provider.AccessToken(ctx)
	if err != nil || got != "synthetic-access" {
		t.Fatalf("AccessToken() = %q, %v", got, err)
	}
	if err := json.Unmarshal([]byte(store["account-key"]), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.RefreshToken != "synthetic-rotated" || tokenRequests != 3 {
		t.Fatalf("refresh was not atomically rotated: state=%+v requests=%d", saved, tokenRequests)
	}
}

func TestOAuthErrorsDoNotLeakResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid_grant","error_description":"secret-token-value"}`)
	}))
	defer server.Close()
	cfg := Config{ClientID: "synthetic-client", TokenURL: server.URL + "/token", DeviceCodeURL: server.URL + "/devicecode", HTTPClient: server.Client()}.normalized()
	_, err := requestToken(context.Background(), cfg, url.Values{"grant_type": {"refresh_token"}})
	if err == nil || strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("error exposed response body: %v", err)
	}
}

func TestCanceledTokenLookupDoesNotAccessKeyring(t *testing.T) {
	store := &trackingStore{}
	provider, err := NewTokenProvider(Config{ClientID: "synthetic-client"}, "account", store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.AccessToken(ctx); err == nil {
		t.Fatal("canceled token lookup succeeded")
	}
	if store.gets != 0 {
		t.Fatalf("canceled lookup accessed keyring %d times", store.gets)
	}
}

func TestDeviceLoginHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/devicecode") {
			close(started)
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "synthetic-device-code", "user_code": "ABCD-EFGH", "verification_uri": "https://example.invalid/device", "expires_in": 60, "interval": 30})
			return
		}
		t.Error("token polling should not run after cancellation")
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- DeviceLogin(ctx, Config{ClientID: "synthetic-client", DeviceCodeURL: server.URL + "/devicecode", TokenURL: server.URL + "/token", HTTPClient: server.Client()}, "account", memoryStore{}, &strings.Builder{})
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DeviceLogin() = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DeviceLogin did not stop after cancellation")
	}
}

func TestOAuthRequestsDoNotFollowRedirects(t *testing.T) {
	var downstreamCalls int
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalls++
		_, _ = fmt.Fprint(w, `{"access_token":"should-not-be-requested","expires_in":3600}`)
	}))
	defer downstream.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, downstream.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	cfg := Config{ClientID: "synthetic-client", TokenURL: server.URL + "/token", DeviceCodeURL: server.URL + "/devicecode", HTTPClient: server.Client()}.normalized()
	if _, err := requestToken(context.Background(), cfg, url.Values{"grant_type": {"refresh_token"}}); err == nil {
		t.Fatal("redirect response accepted")
	}
	if downstreamCalls != 0 {
		t.Fatalf("OAuth POST followed redirect %d times", downstreamCalls)
	}
}

func TestRefreshPreservesRefreshTokenWhenRotationOmitted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-access", "expires_in": 3600})
	}))
	defer server.Close()
	store := memoryStore{}
	state, _ := json.Marshal(tokenState{RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(-time.Minute)})
	store["account"] = string(state)
	provider, err := NewTokenProvider(Config{ClientID: "synthetic-client", TokenURL: server.URL + "/token", DeviceCodeURL: server.URL + "/devicecode", HTTPClient: server.Client()}, "account", store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	var saved tokenState
	if err := json.Unmarshal([]byte(store["account"]), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.RefreshToken != "synthetic-refresh" || saved.AccessToken != "synthetic-access" {
		t.Fatalf("refresh token was lost: %+v", saved)
	}
}
