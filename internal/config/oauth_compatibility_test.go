package config

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestMicrosoftOAuthAccountRetainsExistingKeyringIdentity(t *testing.T) {
	source := SourceConfig{Host: "outlook.office365.com", Username: "fixture@example.invalid", OAuth2: &OAuth2Config{ClientID: "fixture-client"}}
	previous := sha256.Sum256([]byte(source.Host + "\x00" + source.Username + "\x00consumers\x00fixture-client"))
	expected := fmt.Sprintf("source-%x", previous[:12])
	if got := OAuthTokenAccount(source); got != expected {
		t.Fatalf("existing keyring identity changed: %s", got)
	}
	source.OAuth2.Provider = "microsoft"
	if OAuthTokenAccount(source) != expected {
		t.Fatal("explicit Microsoft provider changed identity")
	}
	source.OAuth2.Provider = "google"
	if OAuthTokenAccount(source) == expected {
		t.Fatal("provider identities overlap")
	}
}
