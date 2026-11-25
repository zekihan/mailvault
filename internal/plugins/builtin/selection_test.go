package builtin

import (
	"errors"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestRegistryRejectsUnknownTypesAndInvalidConfiguration(t *testing.T) {
	registry := Registry()
	if _, err := registry.CreateSource("unsupported", nil); err == nil {
		t.Fatal("unknown source accepted")
	} else {
		var unknown plugins.UnknownSourceError
		if !errors.As(err, &unknown) || string(unknown) != "unsupported" {
			t.Fatalf("unexpected source error: %v", err)
		}
	}
	if _, err := registry.CreateTarget("unsupported", nil); err == nil {
		t.Fatal("unknown target accepted")
	} else {
		var unknown plugins.UnknownTargetError
		if !errors.As(err, &unknown) || string(unknown) != "unsupported" {
			t.Fatalf("unexpected target error: %v", err)
		}
	}
	for _, kind := range []string{"imap", "pop3"} {
		t.Run(kind, func(t *testing.T) {
			if _, err := registry.CreateSource(kind, map[string]any{}); err == nil {
				t.Fatal("missing source settings accepted")
			}
		})
	}
}
