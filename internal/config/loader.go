package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"
	"github.com/zekihan/mailvault/internal/plugins"
	pluginconfig "github.com/zekihan/mailvault/internal/plugins/config"
)

// Load reads configuration from file and environment.
func Load(path string) (*Config, error) {
	return load(path, false, "", "", false)
}

// LoadForValidation resolves all declared resources for explicit connectivity
// checks. Read fresh configuration rather than resolving active secrets twice.
func LoadForValidation(path string) (*Config, error) {
	return load(path, true, "", "", false)
}

// LoadSelected resolves credentials only for the named source and target.
func LoadSelected(path, sourceName, targetName string) (*Config, error) {
	return load(path, false, sourceName, targetName, false)
}

// LoadOAuthSource reads one source without resolving unrelated credentials.
func LoadOAuthSource(path, sourceName string) (SourceConfig, error) {
	cfg, err := load(path, false, sourceName, "", true)
	if err != nil {
		return SourceConfig{}, err
	}
	for i := range cfg.Sources {
		source := &cfg.Sources[i]
		if source.Name != sourceName {
			continue
		}
		if err := validateOAuthLoaderSource(source, sourceName); err != nil {
			return SourceConfig{}, err
		}
		return *source, nil
	}
	return SourceConfig{}, fmt.Errorf("source %q not found", sourceName)
}

func validateOAuthLoaderSource(source *SourceConfig, sourceName string) error {
	if source.OAuth2 == nil {
		return fmt.Errorf("source %q has no oauth2 configuration", sourceName)
	}
	if source.PasswordRef != "" {
		return fmt.Errorf("source %q: configure either password_ref or oauth2, not both", sourceName)
	}
	if strings.TrimSpace(source.Host) == "" {
		return fmt.Errorf("source %q: host is required", sourceName)
	}
	if strings.TrimSpace(source.Username) == "" {
		return fmt.Errorf("source %q: username is required", sourceName)
	}
	if !source.UseTLS && !source.StartTLS {
		return fmt.Errorf("source %q: OAuth2 requires TLS (use_tls or start_tls)", sourceName)
	}
	if source.OAuth2.ClientSecret != "" {
		return fmt.Errorf("source %q: oauth2.client_secret is not supported for public OAuth clients", sourceName)
	}
	if source.OAuth2.Provider != "" && source.OAuth2.Provider != "microsoft" && source.OAuth2.Provider != "google" {
		return fmt.Errorf("source %q: oauth2.provider must be microsoft or google", sourceName)
	}
	if source.OAuth2.TokenFile != "" && source.OAuth2.TokenKey != "" {
		return fmt.Errorf("source %q: configure either oauth2.token_file or oauth2.token_key", sourceName)
	}
	return nil
}

func resolveConfigDirectory(viperInstance *viper.Viper) string {
	if used := viperInstance.ConfigFileUsed(); used != "" {
		return filepath.Dir(used)
	}
	return ""
}

func absolutizeSourceOAuthTokenPaths(cfg *Config, configDir string) {
	for i := range cfg.Sources {
		if cfg.Sources[i].OAuth2 != nil && cfg.Sources[i].OAuth2.TokenFile != "" && !filepath.IsAbs(cfg.Sources[i].OAuth2.TokenFile) {
			cfg.Sources[i].OAuth2.TokenFile = filepath.Join(configDir, cfg.Sources[i].OAuth2.TokenFile)
		}
	}
}

func configureViperLocation(viperInstance *viper.Viper, path string) {
	if path != "" {
		viperInstance.SetConfigFile(path)
		return
	}
	viperInstance.AddConfigPath(".")
	viperInstance.AddConfigPath("$HOME/.config/mailvault")
	viperInstance.AddConfigPath("/etc/mailvault")
	viperInstance.SetConfigName("config")
}

func load(path string, resolveAll bool, authSource, authTarget string, skipSourceCredential bool) (*Config, error) {
	viperInstance := viper.New()
	viperInstance.SetConfigType("yaml")
	viperInstance.SetDefault("max_message_bytes", plugins.DefaultMaxMessageBytes)

	configureViperLocation(viperInstance, path)

	viperInstance.SetEnvPrefix("MAILVAULT")
	viperInstance.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viperInstance.AllowEmptyEnv(true)
	viperInstance.AutomaticEnv()

	if err := viperInstance.ReadInConfig(); err != nil {
		if _, ok := errors.AsType[viper.ConfigFileNotFoundError](err); !ok {
			return nil, fmt.Errorf("read config: %w", err)
		}
		// Config file not found is OK if using env vars only.
	}

	var cfg Config
	if err := viperInstance.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	if err := applyIndexedEnvironment(&cfg, viperInstance); err != nil {
		return nil, err
	}
	limit, err := pluginconfig.ParseMessageLimit(map[string]any{"max_message_bytes": viperInstance.Get("max_message_bytes")})
	if err != nil {
		return nil, err
	}
	cfg.MaxMessageBytes = limit
	applySourceDefaults(&cfg, viperInstance)
	configDir := resolveConfigDirectory(viperInstance)
	if configDir != "" {
		absolutizeSourceOAuthTokenPaths(&cfg, configDir)
	}

	// Expand credential references in config.
	if authSource == "" && authTarget == "" {
		if err := expandSelectedCredentialRefsAt(&cfg, resolveAll, configDir); err != nil {
			return nil, fmt.Errorf("expand credentials: %w", err)
		}
	} else if skipSourceCredential {
		if err := expandNamedCredentialRefsAt(&cfg, "", authTarget, configDir); err != nil {
			return nil, fmt.Errorf("expand selected credentials: %w", err)
		}
	} else if err := expandNamedCredentialRefsAt(&cfg, authSource, authTarget, configDir); err != nil {
		return nil, fmt.Errorf("expand selected credentials: %w", err)
	}

	return &cfg, nil
}

func expandNamedCredentialRefsAt(cfg *Config, sourceName, targetName, configDir string) error {
	for i := range cfg.Sources {
		if sourceName == "" || cfg.Sources[i].Name != sourceName {
			continue
		}
		value, err := resolveSourceCredentialRefAt(cfg.Sources[i].PasswordRef, configDir)
		if err != nil {
			return fmt.Errorf("source %q password_ref: %w", cfg.Sources[i].Name, err)
		}
		cfg.Sources[i].PasswordRef = value
	}
	for i := range cfg.Targets {
		if targetName == "" || cfg.Targets[i].Name != targetName {
			continue
		}
		if ref, ok := cfg.Targets[i].Config["credentials_ref"].(string); ok {
			value, err := resolveCredentialRefAt(ref, configDir)
			if err != nil {
				return fmt.Errorf("target %q credentials_ref: %w", cfg.Targets[i].Name, err)
			}
			cfg.Targets[i].Config["credentials_ref"] = value
		}
	}
	return nil
}

// Validate checks the configuration for correctness.
func Validate(cfg *Config) error {
	if err := plugins.ValidateMessageLimit(plugins.MessageLimit(cfg.MaxMessageBytes)); err != nil {
		return err
	}
	if len(cfg.Sources) == 0 {
		return errors.New("at least one source is required")
	}
	if len(cfg.Targets) == 0 {
		return errors.New("at least one target is required")
	}
	if len(cfg.Pairs) == 0 {
		return errors.New("at least one source-target pair is required")
	}

	sourceNames := make(map[string]bool)
	for i := range cfg.Sources {
		if err := validateSourceConfig(&cfg.Sources[i], sourceNames); err != nil {
			return err
		}
	}
	targetNames := make(map[string]bool)
	for i := range cfg.Targets {
		if err := validateTargetConfig(&cfg.Targets[i], targetNames); err != nil {
			return err
		}
	}
	for i := range cfg.Pairs {
		if err := validatePairConfig(&cfg.Pairs[i], i, sourceNames, targetNames); err != nil {
			return err
		}
	}
	return nil
}

func validateSourceCredentialsAndLimits(source *SourceConfig) error {
	if source.OAuth2 == nil && source.PasswordRef == "" {
		return fmt.Errorf("source %q: password_ref is required", source.Name)
	}
	if err := validateSourceOAuth(source); err != nil {
		return err
	}
	if source.MaxListingItems < 0 || source.MaxListingBytes < 0 {
		return fmt.Errorf("source %q: max_listing_items and max_listing_bytes must be >= 0 (zero uses defaults)", source.Name)
	}
	if source.MaxConnections <= 0 {
		return fmt.Errorf("source %q: max_connections must be > 0", source.Name)
	}
	if source.ConnectRetryTimeout < 0 {
		return fmt.Errorf("source %q: connect_retry_timeout must be >= 0 (zero disables retries)", source.Name)
	}
	return nil
}

func validateSourceConfig(source *SourceConfig, sourceNames map[string]bool) error {
	if source.Name == "" {
		return errors.New("source name is required")
	}
	if sourceNames[source.Name] {
		return fmt.Errorf("duplicate source name: %s", source.Name)
	}
	sourceNames[source.Name] = true
	if source.Type != "imap" && source.Type != "pop3" {
		return fmt.Errorf("source %q: unknown type %q (expected imap or pop3)", source.Name, source.Type)
	}
	if source.Host == "" {
		return fmt.Errorf("source %q: host is required", source.Name)
	}
	if source.Port < 1 || source.Port > 65535 {
		return fmt.Errorf("source %q: port must be between 1 and 65535", source.Name)
	}
	if source.Username == "" {
		return fmt.Errorf("source %q: username is required", source.Name)
	}
	return validateSourceCredentialsAndLimits(source)
}

func validateSourceOAuth(source *SourceConfig) error {
	if source.OAuth2 == nil {
		return nil
	}
	if source.PasswordRef != "" {
		return fmt.Errorf("source %q: configure either password_ref or oauth2, not both", source.Name)
	}
	if strings.TrimSpace(source.OAuth2.ClientID) == "" {
		return fmt.Errorf("source %q: oauth2.client_id is required", source.Name)
	}
	if !source.UseTLS && !source.StartTLS {
		return fmt.Errorf("source %q: OAuth2 requires TLS (use_tls or start_tls)", source.Name)
	}
	if source.OAuth2.ClientSecret != "" {
		return fmt.Errorf("source %q: oauth2.client_secret is not supported for public OAuth clients", source.Name)
	}
	if source.OAuth2.Provider != "" && source.OAuth2.Provider != "microsoft" && source.OAuth2.Provider != "google" {
		return fmt.Errorf("source %q: oauth2.provider must be microsoft or google", source.Name)
	}
	if source.OAuth2.TokenFile != "" && source.OAuth2.TokenKey != "" {
		return fmt.Errorf("source %q: configure either oauth2.token_file or oauth2.token_key", source.Name)
	}
	if _, err := NewOAuthTokenProvider(source); err != nil {
		return fmt.Errorf("source %q OAuth2: %w", source.Name, err)
	}
	return nil
}

func validateTargetConfig(targetConfig *TargetConfig, targetNames map[string]bool) error {
	if targetConfig.Name == "" {
		return errors.New("target name is required")
	}
	if targetNames[targetConfig.Name] {
		return fmt.Errorf("duplicate target name: %s", targetConfig.Name)
	}
	targetNames[targetConfig.Name] = true
	if targetConfig.Type != "maildir" && targetConfig.Type != "mbox" && targetConfig.Type != "s3" {
		return fmt.Errorf("target %q: unknown type %q (expected maildir, mbox, or s3)", targetConfig.Name, targetConfig.Type)
	}
	return nil
}

func validatePairConfig(pairCfg *PairConfig, index int, sourceNames, targetNames map[string]bool) error {
	if pairCfg.Source == "" {
		return fmt.Errorf("pair %d: source is required", index)
	}
	if !sourceNames[pairCfg.Source] {
		return fmt.Errorf("pair %d: unknown source %q", index, pairCfg.Source)
	}
	if len(pairCfg.Targets) == 0 {
		return fmt.Errorf("pair %d: at least one target is required", index)
	}
	for _, targetName := range pairCfg.Targets {
		if !targetNames[targetName] {
			return fmt.Errorf("pair %d: unknown target %q", index, targetName)
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
		expanded, err := resolveSourceCredentialRefAt(cfg.Sources[i].PasswordRef, configDir)
		if err != nil {
			return fmt.Errorf("source %q password_ref: %w", cfg.Sources[i].Name, err)
		}
		cfg.Sources[i].PasswordRef = expanded
	}
	for i := range cfg.Targets {
		if !resolveAll && !targets[cfg.Targets[i].Name] {
			continue
		}
		if credRef, ok := cfg.Targets[i].Config["credentials_ref"].(string); ok {
			expanded, err := resolveCredentialRefAt(credRef, configDir)
			if err != nil {
				return fmt.Errorf("target %q credentials_ref: %w", cfg.Targets[i].Name, err)
			}
			cfg.Targets[i].Config["credentials_ref"] = expanded
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
		data, err := readCredentialFile(path)
		if err != nil {
			return "", fmt.Errorf("read file %q: %w", path, err)
		}
		return string(data), nil

	case strings.HasPrefix(ref, "keyring:"):
		parts := strings.SplitN(strings.TrimPrefix(ref, "keyring:"), ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", errors.New("keyring reference must be keyring:service:account")
		}
		if lookup == nil {
			return "", errors.New("keyring lookup is unavailable")
		}
		return lookup(parts[0], parts[1])

	default:
		return ref, nil // Plain value, return as-is
	}
}

func applyIndexedEnvironment(cfg *Config, viperInstance *viper.Viper) error {
	for i := range cfg.Sources {
		if err := applyIndexedSourceEnvironment(cfg, viperInstance, i); err != nil {
			return err
		}
	}
	for i := range cfg.Pairs {
		if err := applyIndexedPairEnvironment(cfg, viperInstance, i); err != nil {
			return err
		}
	}
	for i := range cfg.Targets {
		if err := applyIndexedTargetEnvironment(cfg, viperInstance, i); err != nil {
			return err
		}
	}
	return nil
}

func applyIndexedSourceEnvironment(cfg *Config, viperInstance *viper.Viper, i int) error {
	prefix := fmt.Sprintf("sources.%d.", i)
	bind := func(key string) (bool, string) { return bindEnvironment(viperInstance, prefix+key) }
	applyIndexedSourceStrings(&cfg.Sources[i], bind)
	if err := applyIndexedSourceValues(&cfg.Sources[i], bind, prefix); err != nil {
		return err
	}
	return applyIndexedSourceOAuth(cfg, viperInstance, i, prefix)
}

func applyIndexedSourceStrings(source *SourceConfig, bind func(string) (bool, string)) {
	if set, value := bind("name"); set {
		source.Name = value
	}
	if set, value := bind("type"); set {
		source.Type = value
	}
	if set, value := bind("host"); set {
		source.Host = value
	}
	if set, value := bind("username"); set {
		source.Username = value
	}
	if set, value := bind("password_ref"); set {
		source.PasswordRef = value
	}
}

func applyIndexedSourceValues(source *SourceConfig, bind func(string) (bool, string), prefix string) error {
	if err := applyIndexedSourcePorts(source, bind, prefix); err != nil {
		return err
	}
	if err := applyIndexedSourceTimeouts(source, bind, prefix); err != nil {
		return err
	}
	return applyIndexedSourceListings(source, bind, prefix)
}

func applyIndexedSourcePorts(source *SourceConfig, bind func(string) (bool, string), prefix string) error {
	if set, value := bind("port"); set {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("environment override for %sport: %w", prefix, err)
		}
		source.Port = parsed
	}
	if set, value := bind("use_tls"); set {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("environment override for %suse_tls: %w", prefix, err)
		}
		source.UseTLS = parsed
	}
	if set, value := bind("start_tls"); set {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("environment override for %sstart_tls: %w", prefix, err)
		}
		source.StartTLS = parsed
	}
	if set, value := bind("max_connections"); set {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("environment override for %smax_connections: %w", prefix, err)
		}
		source.MaxConnections = parsed
	}
	return nil
}

func applyIndexedSourceTimeouts(source *SourceConfig, bind func(string) (bool, string), prefix string) error {
	if set, value := bind("connection_timeout"); set {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("environment override for %sconnection_timeout: %w", prefix, err)
		}
		source.ConnectionTimeout = parsed
	}
	if set, value := bind("read_timeout"); set {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("environment override for %sread_timeout: %w", prefix, err)
		}
		source.ReadTimeout = parsed
	}
	if set, value := bind("connect_retry_timeout"); set {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("environment override for %sconnect_retry_timeout: %w", prefix, err)
		}
		source.ConnectRetryTimeout = parsed
	}
	return nil
}

func applyIndexedSourceListings(source *SourceConfig, bind func(string) (bool, string), prefix string) error {
	for key, dst := range map[string]*int{"max_listing_items": &source.MaxListingItems, "max_listing_bytes": &source.MaxListingBytes} {
		if set, value := bind(key); set {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("environment override for %s%s: %w", prefix, key, err)
			}
			*dst = parsed
		}
	}
	if set, value := bind("folders"); set {
		if value == "" {
			source.Folders = []string{}
		} else {
			source.Folders = strings.Split(value, ",")
		}
	}
	return nil
}

func applyIndexedSourceOAuth(cfg *Config, viperInstance *viper.Viper, i int, prefix string) error {
	oauth := cfg.Sources[i].OAuth2
	if oauth == nil {
		if exists, _ := bindEnvironment(viperInstance, prefix+"oauth2.client_id"); exists {
			oauth = &OAuth2Config{}
			cfg.Sources[i].OAuth2 = oauth
		}
	}
	if oauth != nil {
		applyIndexedOAuthFields(oauth, viperInstance, prefix+"oauth2.")
	}
	return nil
}

func applyIndexedOAuthFields(oauth *OAuth2Config, viperInstance *viper.Viper, op string) {
	if set, value := bindEnvironment(viperInstance, op+"provider"); set {
		oauth.Provider = value
	}
	if set, value := bindEnvironment(viperInstance, op+"client_id"); set {
		oauth.ClientID = value
	}
	if set, value := bindEnvironment(viperInstance, op+"client_secret"); set {
		oauth.ClientSecret = value
	}
	if set, value := bindEnvironment(viperInstance, op+"tenant"); set {
		oauth.Tenant = value
	}
	if set, value := bindEnvironment(viperInstance, op+"token_key"); set {
		oauth.TokenKey = value
	}
	if set, value := bindEnvironment(viperInstance, op+"token_file"); set {
		oauth.TokenFile = value
	}
	if set, value := bindEnvironment(viperInstance, op+"authorization_url"); set {
		oauth.AuthorizationURL = value
	}
	if set, value := bindEnvironment(viperInstance, op+"device_code_url"); set {
		oauth.DeviceCodeURL = value
	}
	if set, value := bindEnvironment(viperInstance, op+"token_url"); set {
		oauth.TokenURL = value
	}
	if set, value := bindEnvironment(viperInstance, op+"scopes"); set {
		if value == "" {
			oauth.Scopes = []string{}
		} else {
			oauth.Scopes = strings.Split(value, ",")
		}
	}
}

func applyIndexedPairEnvironment(cfg *Config, viperInstance *viper.Viper, i int) error {
	prefix := fmt.Sprintf("pairs.%d.", i)
	if set, value := bindEnvironment(viperInstance, prefix+"source"); set {
		cfg.Pairs[i].Source = value
	}
	if set, value := bindEnvironment(viperInstance, prefix+"targets"); set {
		if value == "" {
			cfg.Pairs[i].Targets = []string{}
		} else {
			cfg.Pairs[i].Targets = strings.Split(value, ",")
		}
	}
	return nil
}

func applyIndexedTargetEnvironment(cfg *Config, viperInstance *viper.Viper, i int) error {
	prefix := fmt.Sprintf("targets.%d.", i)
	if set, value := bindEnvironment(viperInstance, prefix+"name"); set {
		cfg.Targets[i].Name = value
	}
	if set, value := bindEnvironment(viperInstance, prefix+"type"); set {
		cfg.Targets[i].Type = value
	}
	for key, current := range cfg.Targets[i].Config {
		if err := applyTargetConfigEnvironment(cfg, viperInstance, i, prefix, key, current); err != nil {
			return err
		}
	}
	return nil
}

func applyTargetConfigEnvironment(cfg *Config, viperInstance *viper.Viper, i int, prefix, key string, current any) error {
	set, value := bindEnvironment(viperInstance, prefix+"config."+key)
	if !set {
		return nil
	}
	switch current.(type) {
	case bool:
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("environment override for %sconfig.%s: %w", prefix, key, err)
		}
		cfg.Targets[i].Config[key] = parsed
	case int:
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("environment override for %sconfig.%s: %w", prefix, key, err)
		}
		cfg.Targets[i].Config[key] = parsed
	case float64:
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("environment override for %sconfig.%s: %w", prefix, key, err)
		}
		cfg.Targets[i].Config[key] = parsed
	default:
		cfg.Targets[i].Config[key] = value
	}
	return nil
}

func bindEnvironment(v *viper.Viper, key string) (bool, string) {
	_ = v.BindEnv(key) //nolint:errcheck // dotted config keys cannot fail to bind
	envKey := "MAILVAULT_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
	value, ok := os.LookupEnv(envKey)
	return ok, value
}
