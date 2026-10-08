package config

// Config keys for source factories
const (
	SourceConfigHost              = "host"
	SourceConfigPort              = "port"
	SourceConfigUsername          = "username"
	SourceConfigPasswordRef       = "password_ref"
	SourceConfigUseTLS            = "use_tls"
	SourceConfigStartTLS          = "start_tls"
	SourceConfigFolders           = "folders"
	SourceConfigConnectionTimeout = "connection_timeout"
	SourceConfigReadTimeout       = "read_timeout"
	SourceConfigMaxConnections    = "max_connections"
	SourceConfigOAuth2            = "oauth2"
)

// Config keys for target factories
const (
	TargetConfigPath            = "path"
	TargetConfigCreateIfMissing = "create_if_missing"
	TargetConfigBucket          = "bucket"
	TargetConfigRegion          = "region"
	TargetConfigEndpoint        = "endpoint"
	TargetConfigPrefix          = "prefix"
	TargetConfigCredentialsRef  = "credentials_ref"
)
