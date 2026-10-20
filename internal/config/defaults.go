package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

// Apply defaults while field presence is still available. Zero values in the
// decoded struct alone cannot distinguish omission from an explicit opt-out.
// InConfig checks decoded file presence; IsSet also sees automatic env lookups,
// which may not have been decoded into indexed source entries.
func applySourceDefaults(cfg *Config, values *viper.Viper) {
	for i := range cfg.Sources {
		src := &cfg.Sources[i]
		prefix := fmt.Sprintf("sources.%d.", i)
		if !values.InConfig(prefix + "use_tls") {
			src.UseTLS = !src.StartTLS
		}
		if !values.InConfig(prefix + "connection_timeout") {
			src.ConnectionTimeout = 30 * time.Second
		}
		if !values.InConfig(prefix + "read_timeout") {
			src.ReadTimeout = 60 * time.Second
		}
	}
}
