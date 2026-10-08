package builtin

import (
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/source"
	"github.com/zekihan/mailvault/internal/plugins/target"
)

// Registry returns a registry with all built-in plugins registered.
func Registry() *plugins.Registry {
	r := plugins.NewRegistry()

	// Register built-in sources
	r.RegisterSource("imap", source.NewIMapSource)
	r.RegisterSource("pop3", source.NewPOP3Source)

	// Register built-in targets
	r.RegisterTarget("maildir", target.NewMaildirTarget)
	r.RegisterTarget("mbox", target.NewMboxTarget)
	r.RegisterTarget("s3", target.NewS3Target)

	return r
}
