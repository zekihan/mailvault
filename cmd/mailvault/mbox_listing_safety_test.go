package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
)

func TestVerifyRejectsMalformedMboxWithoutManifest(t *testing.T) {
	configureCLI(t)
	mboxPath := filepath.Join(physicalTempDir(t), "archive.mbox")
	if err := os.WriteFile(mboxPath, []byte("Subject: damaged\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configData, err := json.Marshal(map[string]any{
		"sources": []any{},
		"targets": []any{map[string]any{
			"name": "local", "type": "mbox", "config": map[string]any{"path": mboxPath},
		}},
		"pairs": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgFile, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	jsonOutput = true
	cmd := newVerifyCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetContext(t.Context())
	err = cmd.RunE(cmd, []string{"local"})
	if !errors.Is(err, plugins.ErrBackupMessageCorrupt) {
		t.Fatalf("verify error = %v, want malformed mbox corruption", err)
	}
	if output.Len() != 0 {
		t.Fatalf("verify output = %q, want no clean report", output.String())
	}
}

func TestMboxListingHonorsCanceledContext(t *testing.T) {
	path := filepath.Join(physicalTempDir(t), "archive.mbox")
	if err := os.WriteFile(path, []byte("From sender\nSubject: fixture\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mboxTarget, err := target.NewMboxTarget(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := mustAssert[*target.MboxTarget](t, mboxTarget).ListBackupMessages(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("listing error = %v, want canceled context", err)
	}
}
