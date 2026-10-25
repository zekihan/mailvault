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
	listingLimits   backupListingLimits
	maxMessageBytes int64
	name            string
	path            string
	create          bool
	preserveFolders bool
	preserveFlags   bool
	mu              sync.Mutex
	root            *maildirDirectory
	removeTemp      func(*maildirDirectory, string) error
	link            func(*maildirDirectory, string, *maildirDirectory, string) error
	syncDir         func(string) error
	lastLocator     string
}

func NewMaildirTarget(configMap map[string]interface{}) (plugins.Target, error) {
	limits, err := parseBackupListingLimits(configMap)
	if err != nil {
		return nil, err
	}
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
		listingLimits:   limits,
		maxMessageBytes: maxMessageBytes,
		name:            "maildir:" + path, // Will be overridden by registry
		path:            path,
		create:          create,
		preserveFolders: preserveFolders,
		preserveFlags:   preserveFlags,
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

	if err := ctx.Err(); err != nil {
		return err
	}
	if t.root != nil {
		return t.root.validate()
	}
	var created []maildirCreation
	root, err := openMaildirPath(t.path, t.create, &created)
	if err == nil {
		err = t.ensureMaildir(root, &created)
	}
	if err == nil {
		err = t.syncStructure(created)
	}
	if err != nil {
		if root != nil {
			_ = root.Close()
		}
		err = errors.Join(err, rollbackMaildirStructure(created))
	} else {
		t.root = root
	}
	closeMaildirCreations(created)
	return err
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
	if t.root == nil {
		return "", errors.New("maildir target not initialized")
	}
	if err := t.root.validate(); err != nil {
		return "", err
	}
	messagePath, err := t.messagePath(msg.Folder)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(t.path, messagePath)
	if err != nil {
		return "", err
	}
	var created []maildirCreation
	directory, err := t.root.walk(rel, t.create, &created)
	if err == nil {
		err = t.ensureMaildir(directory, &created)
	}
	if err == nil {
		err = t.syncStructure(created)
	}
	if err != nil {
		if directory != nil {
			_ = directory.Close()
		}
		err = errors.Join(err, rollbackMaildirStructure(created))
		closeMaildirCreations(created)
		return "", err
	}
	closeMaildirCreations(created)
	defer func() { _ = directory.Close() }()
	tmp, err := directory.child("tmp", false, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tmp.Close() }()
	flagSuffix := t.maildirFlags(msg.Flags)
	newDirName := "new"
	if flagSuffix != "" {
		newDirName = "cur"
	}
	published, err := directory.child(newDirName, false, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = published.Close() }()
	file, tmpName, err := tmp.createTemp()
	if err != nil {
		return "", fmt.Errorf("create tmp message: %w", err)
	}
	defer func() {
		remove := t.removeTemp
		if remove == nil {
			remove = removeMaildirFile
		}
		cleanupErr := remove(tmp, tmpName)
		if cleanupErr != nil && err == nil {
			fmt.Fprintf(os.Stderr, "maildir: published message but could not remove temporary file %q: %v\n", tmpName, cleanupErr)
		} else {
			err = errors.Join(err, cleanupErr)
		}
	}()
	_, writeErr := file.Write(msg.Raw)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	expected, statErr := file.Stat()
	if err := errors.Join(writeErr, statErr, file.Close()); err != nil {
		return "", fmt.Errorf("write tmp message: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Publication uses the pinned tmp and destination directories. Replacing a
	// path entry cannot redirect the hard link or its durability operations.
	link := t.link
	if link == nil {
		link = linkMaildirFile
	}
	newName := tmpName + flagSuffix
	if err := validateMaildirDirectories(t.root, directory, tmp, published); err != nil {
		return "", err
	}
	if err := link(tmp, tmpName, published, newName); err != nil {
		return "", fmt.Errorf("publish maildir message: %w", err)
	}
	if err := checkMaildirPublication(published, newName, expected); err != nil {
		return "", errors.Join(err, t.rollbackPublication(published, newName))
	}
	if err := validateMaildirDirectories(t.root, directory, tmp, published); err != nil {
		return "", errors.Join(err, t.rollbackPublication(published, newName))
	}
	if syncErr := t.syncOpenedDirectory(published); syncErr != nil {
		return "", errors.Join(fmt.Errorf("sync published maildir message: %w", syncErr), t.rollbackPublication(published, newName))
	}
	if err := validateMaildirDirectories(t.root, directory, tmp, published); err != nil {
		return "", errors.Join(err, t.rollbackPublication(published, newName))
	}
	locator = filepath.Join(rel, newDirName, newName)
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

func (t *MaildirTarget) ensureMaildir(directory *maildirDirectory, created *[]maildirCreation) error {
	for _, name := range []string{"cur", "new", "tmp"} {
		child, err := directory.child(name, t.create, created)
		if err != nil {
			return fmt.Errorf("maildir directory %q: %w", name, err)
		}
		_ = child.Close()
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
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.root == nil {
		return nil
	}
	err := t.root.Close()
	t.root = nil
	return err
}
