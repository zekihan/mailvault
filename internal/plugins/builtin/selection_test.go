package builtin

import (
	"errors"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestRegistryRejectsUnknownTypesAndInvalidConfiguration(t *testing.T) {
	r := Registry()
	if _, err := r.CreateSource("unsupported", nil); err == nil {
		t.Fatal("unknown source accepted")
	} else {
		var unknown plugins.ErrUnknownSource
		if !errors.As(err, &unknown) || string(unknown) != "unsupported" {
			t.Fatalf("unexpected source error: %v", err)
		}
	}
	if _, err := r.CreateTarget("unsupported", nil); err == nil {
		t.Fatal("unknown target accepted")
	} else {
		var unknown plugins.ErrUnknownTarget
		if !errors.As(err, &unknown) || string(unknown) != "unsupported" {
			t.Fatalf("unexpected target error: %v", err)
		}
	}
	for _, kind := range []string{"imap", "pop3"} {
		t.Run(kind, func(t *testing.T) {
			if _, err := r.CreateSource(kind, map[string]interface{}{}); err == nil {
				t.Fatal("missing source settings accepted")
			}
		})
	}
}
