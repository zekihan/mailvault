package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// Load reads configuration from file and environment.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")

	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.AddConfigPath(".")
		v.AddConfigPath("$HOME/.config/mailvault")
		v.AddConfigPath("/etc/mailvault")
		v.SetConfigName("config")
	}

	v.SetEnvPrefix("MAILVAULT")
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("read config: %w", err)
		}
		// Config file not found is OK if using env vars only
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// Expand credential references in config
	if err := expandCredentialRefs(&cfg); err != nil {
		return nil, fmt.Errorf("expand credentials: %w", err)
	}

	return &cfg, nil
}

// Validate checks the configuration for correctness.
func Validate(cfg *Config) error {
	if len(cfg.Sources) == 0 {
		return fmt.Errorf("at least one source is required")
	}
	if len(cfg.Targets) == 0 {
		return fmt.Errorf("at least one target is required")
	}
	if len(cfg.Pairs) == 0 {
		return fmt.Errorf("at least one source-target pair is required")
	}

	// Build lookup maps
	sourceNames := make(map[string]bool)
	for _, s := range cfg.Sources {
		if s.Name == "" {
			return fmt.Errorf("source name is required")
		}
		if sourceNames[s.Name] {
			return fmt.Errorf("duplicate source name: %s", s.Name)
		}
		sourceNames[s.Name] = true

		if s.Type != "imap" && s.Type != "pop3" {
			return fmt.Errorf("source %q: unknown type %q (expected imap or pop3)", s.Name, s.Type)
		}
		if s.Host == "" {
			return fmt.Errorf("source %q: host is required", s.Name)
		}
		if s.Port == 0 {
			return fmt.Errorf("source %q: port is required", s.Name)
		}
		if s.Username == "" {
			return fmt.Errorf("source %q: username is required", s.Name)
		}
		if s.PasswordRef == "" && s.OAuth2 == nil {
			return fmt.Errorf("source %q: password_ref or oauth2 is required", s.Name)
		}
		if s.MaxConnections <= 0 {
			return fmt.Errorf("source %q: max_connections must be > 0", s.Name)
		}
	}

	targetNames := make(map[string]bool)
	for _, t := range cfg.Targets {
		if t.Name == "" {
			return fmt.Errorf("target name is required")
		}
		if targetNames[t.Name] {
			return fmt.Errorf("duplicate target name: %s", t.Name)
		}
		targetNames[t.Name] = true

		if t.Type != "maildir" && t.Type != "mbox" && t.Type != "s3" {
			return fmt.Errorf("target %q: unknown type %q (expected maildir, mbox, or s3)", t.Name, t.Type)
		}
	}

	for i, p := range cfg.Pairs {
		if p.Source == "" {
			return fmt.Errorf("pair %d: source is required", i)
		}
		if !sourceNames[p.Source] {
			return fmt.Errorf("pair %d: unknown source %q", i, p.Source)
		}
		if len(p.Targets) == 0 {
			return fmt.Errorf("pair %d: at least one target is required", i)
		}
		for _, t := range p.Targets {
			if !targetNames[t] {
				return fmt.Errorf("pair %d: unknown target %q", i, t)
			}
		}
	}

	return nil
}

// expandCredentialRefs resolves env:, file:, keyring: references in config.
func expandCredentialRefs(cfg *Config) error {
	for i := range cfg.Sources {
		if expanded, err := resolveCredentialRef(cfg.Sources[i].PasswordRef); err != nil {
			return fmt.Errorf("source %q password_ref: %w", cfg.Sources[i].Name, err)
		} else {
			cfg.Sources[i].PasswordRef = expanded
		}
		if cfg.Sources[i].OAuth2 != nil && cfg.Sources[i].OAuth2.ClientSecret != "" {
			if expanded, err := resolveCredentialRef(cfg.Sources[i].OAuth2.ClientSecret); err != nil {
				return fmt.Errorf("source %q oauth2.client_secret: %w", cfg.Sources[i].Name, err)
			} else {
				cfg.Sources[i].OAuth2.ClientSecret = expanded
			}
		}
	}
	for i := range cfg.Targets {
		if credRef, ok := cfg.Targets[i].Config["credentials_ref"].(string); ok {
			if expanded, err := resolveCredentialRef(credRef); err != nil {
				return fmt.Errorf("target %q credentials_ref: %w", cfg.Targets[i].Name, err)
			} else {
				cfg.Targets[i].Config["credentials_ref"] = expanded
			}
		}
	}
	return nil
}

// resolveCredentialRef resolves a single credential reference.
// Supported formats:
//   env:VAR_NAME          - read from environment variable
//   file:/path/to/file    - read first line from file
//   keyring:service:account - read from system keyring
func resolveCredentialRef(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}

	if len(ref) < 5 {
		return ref, nil // Not a recognized prefix, return as-is
	}

	switch {
	case ref[:4] == "env:":
		val := os.Getenv(ref[4:])
		if val == "" {
			return "", fmt.Errorf("environment variable %q not set", ref[4:])
		}
		return val, nil

	case ref[:5] == "file:":
		path := ref[5:]
		if !filepath.IsAbs(path) {
			// Relative to config file directory? For now, require absolute.
			return "", fmt.Errorf("file: path must be absolute: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read file %q: %w", path, err)
		}
		return string(data), nil

	case ref[:8] == "keyring:":
		// TODO: implement keyring lookup (libsecret/WinCred)
		return "", fmt.Errorf("keyring: not yet implemented")

	default:
		return ref, nil // Plain value, return as-is
	}
}