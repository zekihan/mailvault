package sync

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/state"
)

func TestMaildirPreservedPlacementsAfterFlatBackup(t *testing.T) {
	root := t.TempDir()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	message := []byte("Message-ID: <same@example.invalid>\r\n\r\nidentical content\r\n")
	source := &fixtureSource{name: "account", messages: []*plugins.Message{
		{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: message},
		{UID: "2", UIDScope: "generation", Folder: "Starred", Raw: message},
	}}
	cfg := &config.Config{
		Sources: []config.SourceConfig{{Name: "account", Type: "fixture", Folders: []string{"INBOX", "Starred"}}},
		Targets: []config.TargetConfig{{Name: "local", Type: "maildir", Config: map[string]interface{}{"path": root}}},
		Pairs:   []config.PairConfig{{Source: "account", Targets: []string{"local"}}},
	}
	registry := plugins.NewRegistry()
	registry.RegisterSource("fixture", func(map[string]interface{}) (plugins.Source, error) { return source, nil })
	registry.RegisterTarget("maildir", target.NewMaildirTarget)
	run := func(wantWritten int) {
		t.Helper()
		engine, err := NewSyncEngine(cfg, store, registry)
		if err != nil {
			t.Fatal(err)
		}
		stats, err := engine.Run(context.Background())
		if err != nil || stats.TotalErrors != 0 || stats.TotalWritten != wantWritten {
			t.Fatalf("run = %+v, %v; want %d written", stats, err, wantWritten)
		}
	}

	run(1)
	flat, err := os.ReadDir(filepath.Join(root, "new"))
	if err != nil || len(flat) != 1 {
		t.Fatalf("flat backup has %d messages, %v; want one", len(flat), err)
	}
	cfg.Targets[0].Config = map[string]interface{}{"path": root, "preserve_folders": true}
	run(2)

	for _, folder := range []string{"INBOX", "Starred"} {
		folderPath := filepath.Join(root, ".folders", base64.RawURLEncoding.EncodeToString([]byte(folder)))
		entries, err := os.ReadDir(filepath.Join(folderPath, "new"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("preserved folder %q has %d messages, %v; want one", folder, len(entries), err)
		}
	}
	run(0)
}
