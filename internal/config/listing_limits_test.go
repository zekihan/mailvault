package config

import (
	"strings"
	"testing"
)

func TestListingLimitsConfigurationReachesSourcePlugin(t *testing.T) {
	path := writeEnvironmentConfig(t)
	t.Setenv("MAILVAULT_SOURCES_0_MAX_LISTING_ITEMS", "1234")
	t.Setenv("MAILVAULT_SOURCES_0_MAX_LISTING_BYTES", "5678")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	values, err := SourcePluginConfig(cfg.Sources[0])
	if err != nil {
		t.Fatal(err)
	}
	if values["max_listing_items"] != 1234 || values["max_listing_bytes"] != 5678 {
		t.Fatalf("listing limits lost: %v %v", values["max_listing_items"], values["max_listing_bytes"])
	}
	t.Setenv("MAILVAULT_SOURCES_0_MAX_LISTING_ITEMS", "-1")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "max_listing_items") {
		t.Fatalf("negative listing budget accepted: %v", err)
	}
	t.Setenv("MAILVAULT_SOURCES_0_MAX_LISTING_ITEMS", "many")
	if _, err := Load(path); err == nil {
		t.Fatal("malformed listing budget accepted")
	}
}
