package builtin

import (
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/source"
	"github.com/zekihan/mailvault/internal/plugins/target"
)

// Registry returns a registry with all built-in plugins registered.
func Registry() *plugins.Registry {
	registry := plugins.NewRegistry()

	// Register built-in sources.
	registry.RegisterSource("imap", source.NewIMapSource)
	registry.RegisterSource("pop3", source.NewPOP3Source)

	// Register built-in targets.
	registry.RegisterTarget("maildir", target.NewMaildirTarget)
	registry.RegisterTarget("mbox", target.NewMboxTarget)
	registry.RegisterTarget("s3", target.NewS3Target)

	return registry
}
