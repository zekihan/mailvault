package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/config"
)

// MaildirTarget implements the Target interface for Maildir format.
type MaildirTarget struct {
	name       string
	path       string
	create     bool
	mu         sync.Mutex
	removeTemp func(string) error
	link       func(string, string) error
	syncDir    func(string) error
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
		name:       "maildir:" + path, // Will be overridden by registry
		path:       path,
		create:     create,
		removeTemp: os.Remove,
		link:       os.Link,
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

	// Create only the requested structure, tracking directories this call owns.
	dirs := []string{t.path, filepath.Join(t.path, "cur"), filepath.Join(t.path, "new"), filepath.Join(t.path, "tmp")}
	var created []string
	if t.create {
		for _, dir := range dirs {
			owned, err := mkdirMaildirPath(dir)
			created = append(created, owned...)
			if err != nil {
				return errors.Join(fmt.Errorf("create maildir %q: %w", dir, err), removeOwnedMaildirDirs(created))
			}
		}
	} else {
		for _, dir := range dirs {
			info, err := os.Stat(dir)
			if err != nil {
				return fmt.Errorf("existing maildir %q: %w", dir, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("maildir path %q is not a directory", dir)
			}
		}
	}
	if err := t.syncStructure(created); err != nil {
		return errors.Join(err, removeOwnedMaildirDirs(created))
	}
	return nil
}

func (t *MaildirTarget) WriteMessage(ctx context.Context, msg *plugins.Message) (err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	removeTemp, link := t.removeTemp, t.link
	if removeTemp == nil {
		removeTemp = os.Remove
	}
	if link == nil {
		link = os.Link
	}
	// CreateTemp reserves the name exclusively, even across processes and when
	// the clock does not advance. Keep mail private with its default 0600 mode.
	file, err := os.CreateTemp(filepath.Join(t.path, "tmp"), "mailvault-*")
	if err != nil {
		return fmt.Errorf("create tmp message: %w", err)
	}
	tmpPath := file.Name()
	defer func() {
		cleanupErr := removeTemp(tmpPath)
		if cleanupErr != nil && err == nil {
			fmt.Fprintf(os.Stderr, "maildir: published message but could not remove temporary file %q: %v\n", tmpPath, cleanupErr)
		} else {
			err = errors.Join(err, cleanupErr)
		}
	}()
	_, writeErr := file.Write(msg.Raw)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("write tmp message: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A hard link atomically publishes the complete message without replacing
	// an existing new/ entry; Rename would silently overwrite on collision.
	newDir := filepath.Join(t.path, "new")
	newPath := filepath.Join(newDir, filepath.Base(tmpPath))
	if err := link(tmpPath, newPath); err != nil {
		return fmt.Errorf("publish maildir message: %w", err)
	}
	if syncErr := t.syncDirectory(newDir); syncErr != nil {
		return errors.Join(fmt.Errorf("sync published maildir message: %w", syncErr), t.rollbackPublication(newPath, newDir))
	}
	return nil
}

func (t *MaildirTarget) Close() error {
	// Nothing to close for Maildir
	return nil
}
