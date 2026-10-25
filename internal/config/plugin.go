package config

import (
	"fmt"

	"github.com/zekihan/mailvault/internal/plugins"
)

// SourcePluginConfig shares credential and protocol setup between sync and validation.
// Constructing the map does not contact an identity provider or read OAuth tokens.
func SourcePluginConfig(source SourceConfig, limits ...int64) (map[string]interface{}, error) {
	if source.OAuth2 != nil && source.PasswordRef != "" {
		return nil, fmt.Errorf("source %q: configure either password_ref or oauth2, not both", source.Name)
	}
	provider, err := NewOAuthTokenProvider(source)
	if err != nil {
		return nil, fmt.Errorf("source %q OAuth2: %w", source.Name, err)
	}
	values := map[string]interface{}{
		"name": source.Name, "host": source.Host, "port": source.Port,
		"username": source.Username, "password_ref": source.PasswordRef,
		"use_tls": source.UseTLS, "start_tls": source.StartTLS, "folders": source.Folders,
		"connection_timeout": source.ConnectionTimeout, "read_timeout": source.ReadTimeout,
		"max_connections": source.MaxConnections,
	}
	values["max_message_bytes"] = pluginMessageLimit(limits)
	if provider != nil {
		values["oauth2_token_provider"] = provider
	}
	return values, nil
}

// TargetPluginConfig gives every operation the same application message limit.
func TargetPluginConfig(target TargetConfig, limits ...int64) map[string]interface{} {
	values := make(map[string]interface{}, len(target.Config)+2)
	for key, value := range target.Config {
		values[key] = value
	}
	values["name"] = target.Name
	values["max_message_bytes"] = pluginMessageLimit(limits)
	return values
}

func pluginMessageLimit(limits []int64) int64 {
	if len(limits) > 0 {
		return plugins.MessageLimit(limits[0])
	}
	return plugins.DefaultMaxMessageBytes
}
