package target

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/config"
)

// MaildirTarget implements the Target interface for Maildir format.
type MaildirTarget struct {
	maxMessageBytes int64
	name            string
	path            string
	create          bool
	preserveFolders bool
	preserveFlags   bool
	mu              sync.Mutex
	removeTemp      func(string) error
	link            func(string, string) error
	syncDir         func(string) error
	lastLocator     string
}

func NewMaildirTarget(configMap map[string]interface{}) (plugins.Target, error) {
	maxMessageBytes, limitErr := config.ParseMessageLimit(configMap)
	if limitErr != nil {
		return nil, limitErr
	}
	path, _ := config.ParseString(configMap, config.TargetConfigPath, "")
	create, _ := config.ParseBool(configMap, config.TargetConfigCreateIfMissing, true)
	preserveFolders, err := config.ParseBool(configMap, config.TargetConfigPreserveFolders, false)
	if err != nil {
		return nil, fmt.Errorf("preserve_folders: %w", err)
	}
	preserveFlags, err := config.ParseBool(configMap, config.TargetConfigPreserveFlags, false)
	if err != nil {
		return nil, fmt.Errorf("preserve_flags: %w", err)
	}
	if preserveFlags && runtime.GOOS == "windows" {
		return nil, errors.New("maildir flag preservation requires ':' in filenames, which Windows does not support; set preserve_flags: false")
	}

	if path == "" {
		return nil, errors.New("path is required")
	}

	// Expand home directory
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}

	return &MaildirTarget{
		maxMessageBytes: maxMessageBytes,
		name:            "maildir:" + path, // Will be overridden by registry
		path:            path,
		create:          create,
		preserveFolders: preserveFolders,
		preserveFlags:   preserveFlags,
		removeTemp:      os.Remove,
		link:            os.Link,
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

func (t *MaildirTarget) DestinationIdentity() string {
	return destinationFingerprint(t.Type(), canonicalDestinationPath(t.path))
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

func (t *MaildirTarget) WriteMessage(ctx context.Context, msg *plugins.Message) error {
	_, err := t.WriteBackupMessage(ctx, msg)
	return err
}

func (t *MaildirTarget) LastBackupLocator() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastLocator
}

func (t *MaildirTarget) WriteRepairMessage(ctx context.Context, msg *plugins.Message) (string, error) {
	return t.WriteBackupMessage(ctx, msg)
}

func (t *MaildirTarget) WriteBackupMessage(ctx context.Context, msg *plugins.Message) (locator string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastLocator = ""
	if err := plugins.CheckMessageSize(int64(len(msg.Raw)), t.maxMessageBytes); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	removeTemp, link := t.removeTemp, t.link
	if removeTemp == nil {
		removeTemp = os.Remove
	}
	if link == nil {
		link = os.Link
	}
	messagePath, err := t.messagePath(msg.Folder)
	if err != nil {
		return "", err
	}
	if messagePath != t.path {
		if err := t.ensureMaildir(messagePath); err != nil {
			return "", fmt.Errorf("create folder maildir: %w", err)
		}
	}
	flagSuffix := t.maildirFlags(msg.Flags)
	newDirName := "new"
	if flagSuffix != "" {
		newDirName = "cur"
	}
	newDir := filepath.Join(messagePath, newDirName)
	// CreateTemp reserves the name exclusively, even across processes and when
	// the clock does not advance. Keep mail private with its default 0600 mode.
	file, err := os.CreateTemp(filepath.Join(messagePath, "tmp"), "mailvault-*")
	if err != nil {
		return "", fmt.Errorf("create tmp message: %w", err)
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
		return "", fmt.Errorf("write tmp message: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// A hard link atomically publishes the complete message without replacing
	// an existing new/ entry; Rename would silently overwrite on collision.
	newPath := filepath.Join(newDir, filepath.Base(tmpPath)+flagSuffix)
	if err := link(tmpPath, newPath); err != nil {
		return "", fmt.Errorf("publish maildir message: %w", err)
	}
	if syncErr := t.syncDirectory(newDir); syncErr != nil {
		return "", errors.Join(fmt.Errorf("sync published maildir message: %w", syncErr), t.rollbackPublication(newPath, newDir))
	}
	locator, err = filepath.Rel(t.path, newPath)
	if err != nil {
		return "", err
	}
	t.lastLocator = filepath.ToSlash(locator)
	return locator, nil
}

// DeliveryIdentity scopes content deduplication to a source folder when folder
// preservation is enabled. Targets without this method retain their old policy.
func (t *MaildirTarget) DeliveryIdentity(msg *plugins.Message, identity string) string {
	if !t.preserveFolders || msg == nil {
		return identity
	}
	return identity + "\x00folder:" + msg.Folder
}

// ReceiptScope separates incremental receipts when preservation settings change.
func (t *MaildirTarget) ReceiptScope() string {
	if !t.preserveFolders && !t.preserveFlags {
		return ""
	}
	return fmt.Sprintf("maildir-preserve-v1:folders=%t:flags=%t", t.preserveFolders, t.preserveFlags)
}

func (t *MaildirTarget) messagePath(folder string) (string, error) {
	if !t.preserveFolders || folder == "" {
		return t.path, nil
	}
	parts := strings.Split(folder, "/")
	path := filepath.Join(t.path, ".folders")
	for _, part := range parts {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(part))
		if encoded == "" {
			encoded = "~"
		}
		if len(encoded) > 240 {
			return "", fmt.Errorf("folder component is too long to encode safely")
		}
		path = filepath.Join(path, encoded)
	}
	return path, nil
}

func (t *MaildirTarget) ensureMaildir(path string) error {
	dirs := []string{path, filepath.Join(path, "cur"), filepath.Join(path, "new"), filepath.Join(path, "tmp")}
	if !t.create {
		for _, dir := range dirs {
			info, err := os.Stat(dir)
			if err != nil {
				return fmt.Errorf("existing maildir %q: %w", dir, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("maildir path %q is not a directory", dir)
			}
		}
		return nil
	}
	var created []string
	for _, dir := range dirs {
		owned, err := mkdirMaildirPath(dir)
		created = append(created, owned...)
		if err != nil {
			return errors.Join(fmt.Errorf("create maildir %q: %w", dir, err), removeOwnedMaildirDirs(created))
		}
	}
	if err := t.syncStructure(created); err != nil {
		return errors.Join(err, removeOwnedMaildirDirs(created))
	}
	return nil
}

func (t *MaildirTarget) maildirFlags(flags []string) string {
	if !t.preserveFlags {
		return ""
	}
	letters := make(map[rune]bool)
	for _, flag := range flags {
		switch strings.ToLower(flag) {
		case "\\seen", "seen":
			letters['S'] = true
		case "\\answered", "answered":
			letters['R'] = true
		case "\\flagged", "flagged":
			letters['F'] = true
		case "\\deleted", "deleted":
			letters['T'] = true
		case "\\draft", "draft":
			letters['D'] = true
		}
	}
	if len(letters) == 0 {
		return ""
	}
	ordered := make([]rune, 0, len(letters))
	for letter := range letters {
		ordered = append(ordered, letter)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	var out strings.Builder
	out.WriteString(":2,")
	for _, letter := range ordered {
		out.WriteRune(letter)
	}
	return out.String()
}

func (t *MaildirTarget) Close() error {
	// Nothing to close for Maildir
	return nil
}
