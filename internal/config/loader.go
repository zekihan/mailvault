package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"
)

// Load reads configuration from file and environment.
func Load(path string) (*Config, error) {
	return load(path, false, "")
}

// LoadForValidation resolves all declared resources for explicit connectivity
// checks. Read fresh configuration rather than resolving active secrets twice.
func LoadForValidation(path string) (*Config, error) {
	return load(path, true, "")
}

// LoadOAuthSource reads one source without resolving unrelated credentials.
func LoadOAuthSource(path, sourceName string) (SourceConfig, error) {
	cfg, err := load(path, false, sourceName)
	if err != nil {
		return SourceConfig{}, err
	}
	for _, source := range cfg.Sources {
		if source.Name == sourceName {
			if source.OAuth2 == nil {
				return SourceConfig{}, fmt.Errorf("source %q has no oauth2 configuration", sourceName)
			}
			if source.PasswordRef != "" {
				return SourceConfig{}, fmt.Errorf("source %q: configure either password_ref or oauth2, not both", sourceName)
			}
			if strings.TrimSpace(source.Host) == "" {
				return SourceConfig{}, fmt.Errorf("source %q: host is required", sourceName)
			}
			if strings.TrimSpace(source.Username) == "" {
				return SourceConfig{}, fmt.Errorf("source %q: username is required", sourceName)
			}
			if !source.UseTLS && !source.StartTLS {
				return SourceConfig{}, fmt.Errorf("source %q: OAuth2 requires TLS (use_tls or start_tls)", sourceName)
			}
			if source.OAuth2.ClientSecret != "" {
				return SourceConfig{}, fmt.Errorf("source %q: oauth2.client_secret is not supported for public device authorization clients", sourceName)
			}
			return source, nil
		}
	}
	return SourceConfig{}, fmt.Errorf("source %q not found", sourceName)
}

func load(path string, resolveAll bool, authSource string) (*Config, error) {
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
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AllowEmptyEnv(true)
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
	if err := applyIndexedEnvironment(&cfg, v); err != nil {
		return nil, err
	}
	applySourceDefaults(&cfg, v)
	configDir := ""
	if used := v.ConfigFileUsed(); used != "" {
		configDir = filepath.Dir(used)
	}

	// Expand credential references in config
	if authSource == "" {
		if err := expandSelectedCredentialRefsAt(&cfg, resolveAll, configDir); err != nil {
			return nil, fmt.Errorf("expand credentials: %w", err)
		}
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
		if s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("source %q: port must be between 1 and 65535", s.Name)
		}
		if s.Username == "" {
			return fmt.Errorf("source %q: username is required", s.Name)
		}
		if s.OAuth2 == nil && s.PasswordRef == "" {
			return fmt.Errorf("source %q: password_ref is required", s.Name)
		}
		if s.OAuth2 != nil {
			if s.PasswordRef != "" {
				return fmt.Errorf("source %q: configure either password_ref or oauth2, not both", s.Name)
			}
			if strings.TrimSpace(s.OAuth2.ClientID) == "" {
				return fmt.Errorf("source %q: oauth2.client_id is required", s.Name)
			}
			if !s.UseTLS && !s.StartTLS {
				return fmt.Errorf("source %q: OAuth2 requires TLS (use_tls or start_tls)", s.Name)
			}
			if s.OAuth2.ClientSecret != "" {
				return fmt.Errorf("source %q: oauth2.client_secret is not supported for public device authorization clients", s.Name)
			}
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
	return expandSelectedCredentialRefs(cfg, true)
}

func expandSelectedCredentialRefs(cfg *Config, resolveAll bool) error {
	return expandSelectedCredentialRefsAt(cfg, resolveAll, "")
}

func expandSelectedCredentialRefsAt(cfg *Config, resolveAll bool, configDir string) error {
	sources, targets := make(map[string]bool), make(map[string]bool)
	for _, pair := range cfg.Pairs {
		sources[pair.Source] = true
		for _, target := range pair.Targets {
			targets[target] = true
		}
	}
	for i := range cfg.Sources {
		if !resolveAll && !sources[cfg.Sources[i].Name] {
			continue
		}
		if expanded, err := resolveSourceCredentialRefAt(cfg.Sources[i].PasswordRef, configDir); err != nil {
			return fmt.Errorf("source %q password_ref: %w", cfg.Sources[i].Name, err)
		} else {
			cfg.Sources[i].PasswordRef = expanded
		}
	}
	for i := range cfg.Targets {
		if !resolveAll && !targets[cfg.Targets[i].Name] {
			continue
		}
		if credRef, ok := cfg.Targets[i].Config["credentials_ref"].(string); ok {
			if expanded, err := resolveCredentialRefAt(credRef, configDir); err != nil {
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
//
//	env:VAR_NAME          - read from environment variable
//	file:/path/to/file    - read file contents (password normalization is source-specific)
//	keyring:service:account - read from system keyring
func resolveCredentialRef(ref string) (string, error) {
	return resolveCredentialRefAt(ref, "")
}

func resolveCredentialRefAt(ref, configDir string) (string, error) {
	return resolveCredentialRefAtWithKeyring(ref, configDir, keyring.Get)
}

func resolveCredentialRefAtWithKeyring(ref, configDir string, lookup func(service, account string) (string, error)) (string, error) {
	if ref == "" {
		return "", nil
	}

	switch {
	case strings.HasPrefix(ref, "env:"):
		val := os.Getenv(ref[4:])
		if val == "" {
			return "", fmt.Errorf("environment variable %q not set", ref[4:])
		}
		return val, nil

	case strings.HasPrefix(ref, "file:"):
		path := ref[5:]
		if !filepath.IsAbs(path) {
			if configDir == "" {
				configDir = "."
			}
			path = filepath.Join(configDir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read file %q: %w", path, err)
		}
		return string(data), nil

	case strings.HasPrefix(ref, "keyring:"):
		parts := strings.SplitN(strings.TrimPrefix(ref, "keyring:"), ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", fmt.Errorf("keyring reference must be keyring:service:account")
		}
		if lookup == nil {
			return "", fmt.Errorf("keyring lookup is unavailable")
		}
		return lookup(parts[0], parts[1])

	default:
		return ref, nil // Plain value, return as-is
	}
}

func applyIndexedEnvironment(cfg *Config, v *viper.Viper) error {
	for i := range cfg.Sources {
		p := fmt.Sprintf("sources.%d.", i)
		bind := func(key string) (bool, string) { return bindEnvironment(v, p+key) }
		if set, value := bind("name"); set {
			cfg.Sources[i].Name = value
		}
		if set, value := bind("type"); set {
			cfg.Sources[i].Type = value
		}
		if set, value := bind("host"); set {
			cfg.Sources[i].Host = value
		}
		if set, value := bind("port"); set {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("environment override for %sport: %w", p, err)
			}
			cfg.Sources[i].Port = parsed
		}
		if set, value := bind("username"); set {
			cfg.Sources[i].Username = value
		}
		if set, value := bind("password_ref"); set {
			cfg.Sources[i].PasswordRef = value
		}
		if set, value := bind("use_tls"); set {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("environment override for %suse_tls: %w", p, err)
			}
			cfg.Sources[i].UseTLS = parsed
		}
		if set, value := bind("start_tls"); set {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("environment override for %sstart_tls: %w", p, err)
			}
			cfg.Sources[i].StartTLS = parsed
		}
		if set, value := bind("connection_timeout"); set {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("environment override for %sconnection_timeout: %w", p, err)
			}
			cfg.Sources[i].ConnectionTimeout = parsed
		}
		if set, value := bind("read_timeout"); set {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("environment override for %sread_timeout: %w", p, err)
			}
			cfg.Sources[i].ReadTimeout = parsed
		}
		if set, value := bind("max_connections"); set {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("environment override for %smax_connections: %w", p, err)
			}
			cfg.Sources[i].MaxConnections = parsed
		}
		if set, value := bind("folders"); set {
			if value == "" {
				cfg.Sources[i].Folders = []string{}
			} else {
				cfg.Sources[i].Folders = strings.Split(value, ",")
			}
		}
		oauth := cfg.Sources[i].OAuth2
		if oauth == nil {
			if exists, _ := bindEnvironment(v, p+"oauth2.client_id"); exists {
				oauth = &OAuth2Config{}
				cfg.Sources[i].OAuth2 = oauth
			}
		}
		if oauth != nil {
			op := p + "oauth2."
			if set, value := bindEnvironment(v, op+"client_id"); set {
				oauth.ClientID = value
			}
			if set, value := bindEnvironment(v, op+"client_secret"); set {
				oauth.ClientSecret = value
			}
			if set, value := bindEnvironment(v, op+"tenant"); set {
				oauth.Tenant = value
			}
			if set, value := bindEnvironment(v, op+"token_key"); set {
				oauth.TokenKey = value
			}
			if set, value := bindEnvironment(v, op+"device_code_url"); set {
				oauth.DeviceCodeURL = value
			}
			if set, value := bindEnvironment(v, op+"token_url"); set {
				oauth.TokenURL = value
			}
			if set, value := bindEnvironment(v, op+"scopes"); set {
				if value == "" {
					oauth.Scopes = []string{}
				} else {
					oauth.Scopes = strings.Split(value, ",")
				}
			}
		}
	}
	for i := range cfg.Pairs {
		p := fmt.Sprintf("pairs.%d.", i)
		if set, value := bindEnvironment(v, p+"source"); set {
			cfg.Pairs[i].Source = value
		}
		if set, value := bindEnvironment(v, p+"targets"); set {
			if value == "" {
				cfg.Pairs[i].Targets = []string{}
			} else {
				cfg.Pairs[i].Targets = strings.Split(value, ",")
			}
		}
	}
	for i := range cfg.Targets {
		p := fmt.Sprintf("targets.%d.", i)
		if set, value := bindEnvironment(v, p+"name"); set {
			cfg.Targets[i].Name = value
		}
		if set, value := bindEnvironment(v, p+"type"); set {
			cfg.Targets[i].Type = value
		}
		for key, current := range cfg.Targets[i].Config {
			if set, value := bindEnvironment(v, p+"config."+key); set {
				switch current.(type) {
				case bool:
					parsed, err := strconv.ParseBool(value)
					if err != nil {
						return fmt.Errorf("environment override for %sconfig.%s: %w", p, key, err)
					}
					cfg.Targets[i].Config[key] = parsed
				case int:
					parsed, err := strconv.Atoi(value)
					if err != nil {
						return fmt.Errorf("environment override for %sconfig.%s: %w", p, key, err)
					}
					cfg.Targets[i].Config[key] = parsed
				case float64:
					parsed, err := strconv.ParseFloat(value, 64)
					if err != nil {
						return fmt.Errorf("environment override for %sconfig.%s: %w", p, key, err)
					}
					cfg.Targets[i].Config[key] = parsed
				default:
					cfg.Targets[i].Config[key] = value
				}
			}
		}
	}
	return nil
}

func bindEnvironment(v *viper.Viper, key string) (bool, string) {
	_ = v.BindEnv(key)
	envKey := "MAILVAULT_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
	value, ok := os.LookupEnv(envKey)
	return ok, value
}
