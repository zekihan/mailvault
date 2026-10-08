package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/config"
)

// MaildirTarget implements the Target interface for Maildir format.
type MaildirTarget struct {
	name   string
	path   string
	create bool
	mu     sync.Mutex
}

func NewMaildirTarget(configMap map[string]interface{}) (plugins.Target, error) {
	path, _ := config.ParseString(configMap, config.TargetConfigPath, "")
	create, _ := config.ParseBool(configMap, config.TargetConfigCreateIfMissing, true)

	if path == "" {
		return nil, errors.New("path is required")
	}

	// Expand home directory
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}

	return &MaildirTarget{
		name:   "maildir:" + path, // Will be overridden by registry
		path:   path,
		create: create,
	}, nil
}

// SetName sets the target name (called by registry).
func (t *MaildirTarget) SetName(name string) {
	t.name = name
}

func (t *MaildirTarget) Name() string {
	return t.name
}

func (t *MaildirTarget) Type() string {
	return "maildir"
}

func (t *MaildirTarget) Initialize(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Create Maildir directory structure: cur/, new/, tmp/
	dirs := []string{
		t.path,
		filepath.Join(t.path, "cur"),
		filepath.Join(t.path, "new"),
		filepath.Join(t.path, "tmp"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("create maildir %q: %w", dir, err)
		}
	}

	return nil
}

func (t *MaildirTarget) WriteMessage(ctx context.Context, msg *plugins.Message) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Generate unique filename: timestamp.pid.hostname.random
	hostname, _ := os.Hostname()
	filename := fmt.Sprintf("%d.%d.%s.%d",
		time.Now().Unix(),
		os.Getpid(),
		hostname,
		time.Now().UnixNano()%1000000,
	)

	// Write to tmp/ first, then move to new/
	tmpPath := filepath.Join(t.path, "tmp", filename)
	newPath := filepath.Join(t.path, "new", filename)

	if err := os.WriteFile(tmpPath, msg.Raw, 0640); err != nil {
		return fmt.Errorf("write tmp file: %w", err)
	}

	if err := os.Rename(tmpPath, newPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("move to new: %w", err)
	}

	return nil
}

func (t *MaildirTarget) Close() error {
	// Nothing to close for Maildir
	return nil
}
