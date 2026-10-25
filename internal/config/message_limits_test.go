package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestLoadMessageLimit(t *testing.T) {
	for _, value := range []string{"0", "-1", "536870913", "1.5", "true", "invalid"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("max_message_bytes: "+value+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid message limit accepted")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.MaxMessageBytes != plugins.DefaultMaxMessageBytes {
		t.Fatalf("default limit: %+v, %v", cfg, err)
	}
	t.Setenv("MAILVAULT_MAX_MESSAGE_BYTES", "1024")
	cfg, err = Load(path)
	if err != nil || cfg.MaxMessageBytes != 1024 {
		t.Fatalf("env limit: %+v, %v", cfg, err)
	}
	t.Setenv("MAILVAULT_MAX_MESSAGE_BYTES", "0")
	if _, err := Load(path); err == nil {
		t.Fatal("zero env override accepted")
	}
}

func TestPluginMessageLimitsUseGlobalSetting(t *testing.T) {
	source, err := SourcePluginConfig(SourceConfig{}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	original := map[string]interface{}{"max_message_bytes": int64(2048), "path": "fixture"}
	target := TargetPluginConfig(TargetConfig{Name: "backup", Config: original}, 1024)
	if source["max_message_bytes"] != int64(1024) || target["max_message_bytes"] != int64(1024) {
		t.Fatalf("limits differ: source=%v target=%v", source, target)
	}
	if original["max_message_bytes"] != int64(2048) {
		t.Fatal("plugin config mutated original target map")
	}
}
