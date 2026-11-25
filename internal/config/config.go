package config

import (
	"time"
)

// Config is the root configuration structure.
type Config struct {
	MaxMessageBytes int64          `json:"MaxMessageBytes" yaml:"max_message_bytes" mapstructure:"max_message_bytes"`
	Sources         []SourceConfig `json:"Sources"         yaml:"sources"           mapstructure:"sources"`
	Targets         []TargetConfig `json:"Targets"         yaml:"targets"           mapstructure:"targets"`
	Pairs           []PairConfig   `json:"Pairs"           yaml:"pairs"             mapstructure:"pairs"`
}

// SourceConfig defines a mail source (IMAP/POP3).
type SourceConfig struct {
	Name              string        `json:"Name"              yaml:"name"               mapstructure:"name"`
	Type              string        `json:"Type"              yaml:"type"               mapstructure:"type"` // "imap" or "pop3"
	Host              string        `json:"Host"              yaml:"host"               mapstructure:"host"`
	Port              int           `json:"Port"              yaml:"port"               mapstructure:"port"`
	Username          string        `json:"Username"          yaml:"username"           mapstructure:"username"`
	PasswordRef       string        `json:"PasswordRef"       yaml:"password_ref"       mapstructure:"password_ref"`
	UseTLS            bool          `json:"UseTLS"            yaml:"use_tls"            mapstructure:"use_tls"`
	StartTLS          bool          `json:"StartTLS"          yaml:"start_tls"          mapstructure:"start_tls"`
	Folders           []string      `json:"Folders"           yaml:"folders"            mapstructure:"folders"`
	ConnectionTimeout time.Duration `json:"ConnectionTimeout" yaml:"connection_timeout" mapstructure:"connection_timeout"`
	ReadTimeout       time.Duration `json:"ReadTimeout"       yaml:"read_timeout"       mapstructure:"read_timeout"`
	MaxConnections    int           `json:"MaxConnections"    yaml:"max_connections"    mapstructure:"max_connections"`
	// ConnectRetryTimeout bounds IMAP initial connection retries; zero disables them.
	ConnectRetryTimeout time.Duration `json:"ConnectRetryTimeout" yaml:"connect_retry_timeout" mapstructure:"connect_retry_timeout"`

	MaxListingItems int `json:"MaxListingItems" yaml:"max_listing_items" mapstructure:"max_listing_items"`
	MaxListingBytes int `json:"MaxListingBytes" yaml:"max_listing_bytes" mapstructure:"max_listing_bytes"`

	// OAuth2 (optional).
	OAuth2 *OAuth2Config `json:"OAuth2" yaml:"oauth2" mapstructure:"oauth2"`
}

// OAuth2Config holds OAuth2 configuration for sources that support it.
type OAuth2Config struct {
	Provider         string   `json:"Provider"         yaml:"provider"          mapstructure:"provider"`
	ClientID         string   `json:"ClientID"         yaml:"client_id"         mapstructure:"client_id"`
	ClientSecret     string   `json:"ClientSecret"     yaml:"client_secret"     mapstructure:"client_secret"`
	Tenant           string   `json:"Tenant"           yaml:"tenant"            mapstructure:"tenant"`
	TokenKey         string   `json:"TokenKey"         yaml:"token_key"         mapstructure:"token_key"`
	TokenFile        string   `json:"TokenFile"        yaml:"token_file"        mapstructure:"token_file"`
	DeviceCodeURL    string   `json:"DeviceCodeURL"    yaml:"device_code_url"   mapstructure:"device_code_url"`
	AuthorizationURL string   `json:"AuthorizationURL" yaml:"authorization_url" mapstructure:"authorization_url"`
	TokenURL         string   `json:"TokenURL"         yaml:"token_url"         mapstructure:"token_url"`
	Scopes           []string `json:"Scopes"           yaml:"scopes"            mapstructure:"scopes"`
}

// TargetConfig defines a mail target (local filesystem, S3, etc.).
type TargetConfig struct {
	Name   string         `json:"Name"   yaml:"name"   mapstructure:"name"`
	Type   string         `json:"Type"   yaml:"type"   mapstructure:"type"` // "maildir", "mbox", "s3"
	Config map[string]any `json:"Config" yaml:"config" mapstructure:"config"`
}

// PairConfig defines an explicit source->targets mapping.
type PairConfig struct {
	Source  string   `json:"Source"  yaml:"source"  mapstructure:"source"`
	Targets []string `json:"Targets" yaml:"targets" mapstructure:"targets"`
}
