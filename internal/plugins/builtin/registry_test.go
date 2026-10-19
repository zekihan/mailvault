package builtin

import (
	"path/filepath"
	"testing"
)

func TestRegistryAppliesSourceNames(t *testing.T) {
	for _, kind := range []string{"imap", "pop3"} {
		t.Run(kind, func(t *testing.T) {
			src, err := Registry().CreateSource(kind, map[string]interface{}{
				"name": "configured-source", "host": "example.invalid",
				"username": "test-user", "password_ref": "synthetic-value",
			})
			if err != nil {
				t.Fatal(err)
			}
			if src.Name() != "configured-source" {
				t.Fatalf("Name() = %q, want configured-source", src.Name())
			}
		})
	}
}

func TestRegistryAppliesTargetNames(t *testing.T) {
	for _, kind := range []string{"maildir", "mbox", "s3"} {
		t.Run(kind, func(t *testing.T) {
			tgt, err := Registry().CreateTarget(kind, map[string]interface{}{
				"name": "configured-target", "path": filepath.Join(t.TempDir(), "mail"),
				"bucket": "test-bucket", "region": "us-east-1", "credentials_ref": "synthetic-value",
			})
			if err != nil {
				t.Fatal(err)
			}
			if tgt.Name() != "configured-target" {
				t.Fatalf("Name() = %q, want configured-target", tgt.Name())
			}
		})
	}
}

func TestRegistryPreservesDefaultName(t *testing.T) {
	src, err := Registry().CreateSource("imap", map[string]interface{}{
		"host": "example.invalid", "username": "test-user", "password_ref": "synthetic-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	if src.Name() != "imap:example.invalid" {
		t.Fatalf("Name() = %q, want imap:example.invalid", src.Name())
	}
}
