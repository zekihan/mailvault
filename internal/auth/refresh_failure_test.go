package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRefreshFailurePreservesStoredToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer server.Close()
	encoded, err := json.Marshal(tokenState{AccessToken: "expired", RefreshToken: "fixture", Expiry: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	store := memoryStore{"account": string(encoded)}
	provider, err := NewTokenProvider(&Config{ClientID: "client", TokenURL: server.URL, HTTPClient: server.Client()}, "account", store)
	if err != nil {
		t.Fatal(err)
	}
	if token, err := provider.AccessToken(t.Context()); err == nil || token != "" {
		t.Fatalf("AccessToken = %q, %v", token, err)
	}
	if store["account"] != string(encoded) {
		t.Fatal("failed refresh changed stored credentials")
	}
}
