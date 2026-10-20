package config

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/zekihan/mailvault/internal/auth"
)

// NewOAuthTokenProvider builds a source token callback without contacting the
// identity provider. AccessToken refreshes lazily when a connection needs it.
func NewOAuthTokenProvider(source SourceConfig) (func(context.Context) (string, error), error) {
	if source.OAuth2 == nil {
		return nil, nil
	}
	if !source.UseTLS && !source.StartTLS {
		return nil, fmt.Errorf("source %q: OAuth2 requires TLS (use_tls or start_tls)", source.Name)
	}
	options := source.OAuth2
	if options.ClientSecret != "" {
		return nil, fmt.Errorf("source %q: oauth2.client_secret is not supported for public OAuth clients", source.Name)
	}
	store := oauthStore(options)
	provider, err := auth.NewTokenProvider(auth.Config{
		Provider: options.Provider, ClientID: options.ClientID, Tenant: options.Tenant, Scopes: options.Scopes,
		DeviceCodeURL: options.DeviceCodeURL, AuthorizationURL: options.AuthorizationURL, TokenURL: options.TokenURL,
	}, OAuthTokenAccount(source), store)
	if err != nil {
		return nil, err
	}
	return provider.AccessToken, nil
}

// OAuthTokenAccount returns the keyring account used by login and sync.
func OAuthTokenAccount(source SourceConfig) string {
	if source.OAuth2 != nil && source.OAuth2.TokenKey != "" {
		return source.OAuth2.TokenKey
	}
	tenant, clientID, provider := "consumers", "", "microsoft"
	if source.OAuth2 != nil {
		if source.OAuth2.Provider != "" {
			provider = source.OAuth2.Provider
		}
		if source.OAuth2.Tenant != "" {
			tenant = source.OAuth2.Tenant
		}
		clientID = source.OAuth2.ClientID
	}
	identity := source.Host + "\x00" + source.Username + "\x00" + tenant + "\x00" + clientID
	if provider != "microsoft" {
		identity = provider + "\x00" + identity
	}
	hash := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("source-%x", hash[:12])
}

func oauthStore(options *OAuth2Config) auth.Store {
	if options.TokenFile != "" {
		return auth.FileStore{Path: options.TokenFile}
	}
	return auth.KeyringStore{}
}
