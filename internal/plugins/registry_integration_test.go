package plugins_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/builtin"
)

func TestBuiltinRegistrySelection(t *testing.T) {
	r := builtin.Registry()
	for _, kind := range []string{"imap", "pop3"} {
		src, err := r.CreateSource(kind, map[string]interface{}{"name": "account", "host": "example.invalid", "username": "user", "password_ref": "fixture"})
		if err != nil || src.Type() != kind || src.Name() != "account" {
			t.Fatalf("source %s: %v, %v", kind, src, err)
		}
	}
	for _, kind := range []string{"maildir", "mbox"} {
		tgt, err := r.CreateTarget(kind, map[string]interface{}{"name": "archive", "path": filepath.Join(t.TempDir(), "mail")})
		if err != nil || tgt.Type() != kind || tgt.Name() != "archive" {
			t.Fatalf("target %s: %v, %v", kind, tgt, err)
		}
	}
	if _, err := r.CreateSource("unknown", nil); err == nil {
		t.Fatal("unknown source accepted")
	} else {
		var e plugins.ErrUnknownSource
		if !errors.As(err, &e) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if _, err := r.CreateTarget("unknown", nil); err == nil {
		t.Fatal("unknown target accepted")
	} else {
		var e plugins.ErrUnknownTarget
		if !errors.As(err, &e) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if _, err := r.CreateSource("imap", nil); err == nil {
		t.Fatal("invalid source accepted")
	}
	if _, err := r.CreateTarget("maildir", nil); err == nil {
		t.Fatal("invalid target accepted")
	}
	if len(r.SourceTypes()) != 2 || len(r.TargetTypes()) != 3 {
		t.Fatalf("unexpected registry types: %v, %v", r.SourceTypes(), r.TargetTypes())
	}
}
