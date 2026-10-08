package config

import (
	"time"
)

// Config is the root configuration structure.
type Config struct {
	Sources []SourceConfig `mapstructure:"sources" yaml:"sources"`
	Targets []TargetConfig `mapstructure:"targets" yaml:"targets"`
	Pairs   []PairConfig   `mapstructure:"pairs" yaml:"pairs"`
}

// SourceConfig defines a mail source (IMAP/POP3).
type SourceConfig struct {
	Name              string        `mapstructure:"name" yaml:"name"`
	Type              string        `mapstructure:"type" yaml:"type"` // "imap" or "pop3"
	Host              string        `mapstructure:"host" yaml:"host"`
	Port              int           `mapstructure:"port" yaml:"port"`
	Username          string        `mapstructure:"username" yaml:"username"`
	PasswordRef       string        `mapstructure:"password_ref" yaml:"password_ref"`
	UseTLS            bool          `mapstructure:"use_tls" yaml:"use_tls"`
	StartTLS          bool          `mapstructure:"start_tls" yaml:"start_tls"`
	Folders           []string      `mapstructure:"folders" yaml:"folders"`
	ConnectionTimeout time.Duration `mapstructure:"connection_timeout" yaml:"connection_timeout"`
	ReadTimeout       time.Duration `mapstructure:"read_timeout" yaml:"read_timeout"`
	MaxConnections    int           `mapstructure:"max_connections" yaml:"max_connections"`

	// OAuth2 (optional)
	OAuth2 *OAuth2Config `mapstructure:"oauth2" yaml:"oauth2"`
}

// OAuth2Config holds OAuth2 configuration for sources that support it.
type OAuth2Config struct {
	ClientID     string `mapstructure:"client_id" yaml:"client_id"`
	ClientSecret string `mapstructure:"client_secret" yaml:"client_secret"`
	TokenURL     string `mapstructure:"token_url" yaml:"token_url"`
	Scopes       []string `mapstructure:"scopes" yaml:"scopes"`
}

// TargetConfig defines a mail target (local filesystem, S3, etc.).
type TargetConfig struct {
	Name   string                 `mapstructure:"name" yaml:"name"`
	Type   string                 `mapstructure:"type" yaml:"type"` // "maildir", "mbox", "s3"
	Config map[string]interface{} `mapstructure:"config" yaml:"config"`
}

// PairConfig defines an explicit source->targets mapping.
type PairConfig struct {
	Source  string   `mapstructure:"source" yaml:"source"`
	Targets []string `mapstructure:"targets" yaml:"targets"`
}

// MaildirTargetConfig holds configuration for a Maildir target.
type MaildirTargetConfig struct {
	Path            string `mapstructure:"path" yaml:"path"`
	CreateIfMissing bool   `mapstructure:"create_if_missing" yaml:"create_if_missing"`
}

// MboxTargetConfig holds configuration for an mbox target.
type MboxTargetConfig struct {
	Path            string `mapstructure:"path" yaml:"path"`
	CreateIfMissing bool   `mapstructure:"create_if_missing" yaml:"create_if_missing"`
}

// S3TargetConfig holds configuration for an S3-compatible target.
type S3TargetConfig struct {
	Bucket        string `mapstructure:"bucket" yaml:"bucket"`
	Region        string `mapstructure:"region" yaml:"region"`
	Endpoint      string `mapstructure:"endpoint" yaml:"endpoint"`
	Prefix        string `mapstructure:"prefix" yaml:"prefix"`
	CredentialsRef string `mapstructure:"credentials_ref" yaml:"credentials_ref"`
}