package main

import "github.com/zekihan/mailvault/internal/config"

// listingConfig returns a redacted copy; operational credentials are never changed.
func listingConfig(cfg *config.Config) *config.Config {
	result := *cfg
	result.Sources = append([]config.SourceConfig(nil), cfg.Sources...)
	for i := range result.Sources {
		src := &result.Sources[i]
		if src.PasswordRef != "" {
			src.PasswordRef = "[REDACTED]"
		}
		if src.OAuth2 != nil {
			oauth := *src.OAuth2
			if oauth.ClientSecret != "" {
				oauth.ClientSecret = "[REDACTED]"
			}
			src.OAuth2 = &oauth
		}
	}
	result.Targets = append([]config.TargetConfig(nil), cfg.Targets...)
	for i := range result.Targets {
		values := make(map[string]interface{}, len(cfg.Targets[i].Config))
		for key, value := range cfg.Targets[i].Config {
			if key == "credentials_ref" {
				values[key] = "[REDACTED]"
			} else {
				values[key] = value
			}
		}
		result.Targets[i].Config = values
	}
	return &result
}
