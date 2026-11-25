package plugins_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/builtin"
)

func checkBuiltinSources(t *testing.T, registry *plugins.Registry) {
	t.Helper()
	for _, kind := range []string{"imap", "pop3"} {
		src, err := registry.CreateSource(kind, map[string]any{"name": "account", "host": "example.invalid", "username": "user", "password_ref": "fixture"})
		if err != nil || src.Type() != kind || src.Name() != "account" {
			t.Fatalf("source %s: %v, %v", kind, src, err)
		}
	}
}

func checkBuiltinTargets(t *testing.T, registry *plugins.Registry) {
	t.Helper()
	for _, kind := range []string{"maildir", "mbox"} {
		tgt, err := registry.CreateTarget(kind, map[string]any{"name": "archive", "path": filepath.Join(t.TempDir(), "mail")})
		if err != nil || tgt.Type() != kind || tgt.Name() != "archive" {
			t.Fatalf("target %s: %v, %v", kind, tgt, err)
		}
	}
}

func checkUnknownRegistryKinds(t *testing.T, registry *plugins.Registry) {
	t.Helper()
	_, err := registry.CreateSource("unknown", nil)
	if err == nil {
		t.Fatal("unknown source accepted")
	} else if _, ok := errors.AsType[plugins.UnknownSourceError](err); !ok {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = registry.CreateTarget("unknown", nil)
	if err == nil {
		t.Fatal("unknown target accepted")
	} else if _, ok := errors.AsType[plugins.UnknownTargetError](err); !ok {
		t.Fatalf("unexpected error: %v", err)
	}
}

func checkInvalidRegistryConfigs(t *testing.T, registry *plugins.Registry) {
	t.Helper()
	if _, err := registry.CreateSource("imap", nil); err == nil {
		t.Fatal("invalid source accepted")
	}
	if _, err := registry.CreateTarget("maildir", nil); err == nil {
		t.Fatal("invalid target accepted")
	}
}

func TestBuiltinRegistrySelection(t *testing.T) {
	registry := builtin.Registry()
	checkBuiltinSources(t, registry)
	checkBuiltinTargets(t, registry)
	checkUnknownRegistryKinds(t, registry)
	checkInvalidRegistryConfigs(t, registry)
	if len(registry.SourceTypes()) != 2 || len(registry.TargetTypes()) != 3 {
		t.Fatalf("unexpected registry types: %v, %v", registry.SourceTypes(), registry.TargetTypes())
	}
}
