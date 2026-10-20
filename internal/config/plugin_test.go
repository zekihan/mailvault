package config

import (
	"context"
	"testing"
)

func TestSourcePluginConfigBuildsOAuthWithoutReadingTokens(t *testing.T) {
	source := SourceConfig{Name: "outlook", UseTLS: true, Host: "outlook.office365.com", Username: "fixture@example.invalid", OAuth2: &OAuth2Config{ClientID: "synthetic-client"}}
	values, err := SourcePluginConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values["oauth2_token_provider"].(func(context.Context) (string, error)); !ok {
		t.Fatal("missing OAuth callback")
	}
	if values["name"] != source.Name || values["username"] != source.Username {
		t.Fatal("source identity was not passed through")
	}
	source.PasswordRef = "synthetic-password"
	if _, err := SourcePluginConfig(source); err == nil {
		t.Fatal("ambiguous credentials accepted")
	}
	source.OAuth2 = nil
	values, err = SourcePluginConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := values["oauth2_token_provider"]; exists {
		t.Fatal("password source received OAuth callback")
	}
}
