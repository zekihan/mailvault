package target

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/config"
)

const maildirMaxEncodedComponentBytes = 240

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

func NewMaildirTarget(configMap map[string]any) (plugins.Target, error) {
	limits, err := parseBackupListingLimits(configMap)
	if err != nil {
		return nil, err
	}
	maxMessageBytes, limitErr := config.ParseMessageLimit(configMap)
	if limitErr != nil {
		return nil, limitErr
	}
	path, err := config.ParseString(configMap, config.TargetConfigPath, "")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.TargetConfigPath, err)
	}
	create, err := config.ParseBool(configMap, config.TargetConfigCreateIfMissing, true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.TargetConfigCreateIfMissing, err)
	}
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

	// Expand home directory.
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("expand home directory: %w", err)
		}
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
func (target *MaildirTarget) SetName(name string) {
	target.name = name
}

func (target *MaildirTarget) Name() string {
	return target.name
}

func (target *MaildirTarget) Type() string {
	return "maildir"
}

func (target *MaildirTarget) DestinationIdentity() string {
	return destinationFingerprint(target.Type(), canonicalDestinationPath(target.path))
}

func (target *MaildirTarget) Initialize(ctx context.Context) error {
	target.mu.Lock()
	defer target.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	if target.root != nil {
		return target.root.validate()
	}
	var created []maildirCreation
	root, err := openMaildirPath(target.path, target.create, &created)
	if err == nil {
		err = target.ensureMaildir(root, &created)
	}
	if err == nil {
		err = target.syncStructure(created)
	}
	if err != nil {
		if root != nil {
			_ = root.Close()
		}
		err = errors.Join(err, rollbackMaildirStructure(created))
	} else {
		target.root = root
	}
	closeMaildirCreations(created)
	return err
}

func (target *MaildirTarget) WriteMessage(ctx context.Context, msg *plugins.Message) error {
	_, err := target.WriteBackupMessage(ctx, msg)
	return err
}

func (target *MaildirTarget) LastBackupLocator() string {
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.lastLocator
}

func (target *MaildirTarget) WriteRepairMessage(ctx context.Context, msg *plugins.Message) (string, error) {
	return target.WriteBackupMessage(ctx, msg)
}

func (target *MaildirTarget) WriteBackupMessage(ctx context.Context, msg *plugins.Message) (_ string, err error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.lastLocator = ""
	if err := plugins.CheckMessageSize(int64(len(msg.Raw)), target.maxMessageBytes); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if target.root == nil {
		return "", errors.New("maildir target not initialized")
	}
	if err := target.root.validate(); err != nil {
		return "", err
	}
	rel, directory, err := target.prepareBackupDirectory(msg.Folder)
	if err != nil {
		return "", err
	}
	defer func() { _ = directory.Close() }()
	tmp, published, newDirName, flagSuffix, err := target.openBackupDirectories(directory, msg.Flags)
	if err != nil {
		return "", err
	}
	defer func() { _ = tmp.Close() }()
	defer func() { _ = published.Close() }()
	file, tmpName, err := target.createBackupTemp(tmp)
	if err != nil {
		return "", err
	}
	defer func() {
		if cleanupErr := target.cleanupBackupTemp(tmp, tmpName); cleanupErr != nil {
			if err == nil {
				fmt.Fprintf(os.Stderr, "maildir: published message but could not remove temporary file %q: %v\n", tmpName, cleanupErr)
			} else {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()
	expected, err := writeBackupTemp(ctx, file, msg.Raw)
	if err != nil {
		return "", err
	}
	return target.publishBackupMessage(rel, newDirName, directory, tmp, published, tmpName, flagSuffix, expected)
}

func writeBackupTemp(ctx context.Context, file *os.File, raw []byte) (os.FileInfo, error) {
	_, writeErr := file.Write(raw)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	expected, statErr := file.Stat()
	if err := errors.Join(writeErr, statErr, file.Close()); err != nil {
		return nil, fmt.Errorf("write tmp message: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return expected, nil
}

// DeliveryIdentity scopes content deduplication to a source folder when folder
// preservation is enabled. Targets without this method retain their old policy.
func (target *MaildirTarget) DeliveryIdentity(msg *plugins.Message, identity string) string {
	if !target.preserveFolders || msg == nil {
		return identity
	}
	return identity + "\x00folder:" + msg.Folder
}

// ReceiptScope separates incremental receipts when preservation settings change.
func (target *MaildirTarget) ReceiptScope() string {
	if !target.preserveFolders && !target.preserveFlags {
		return ""
	}
	return fmt.Sprintf("maildir-preserve-v1:folders=%t:flags=%t", target.preserveFolders, target.preserveFlags)
}

func (target *MaildirTarget) Close() error {
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.root == nil {
		return nil
	}
	err := target.root.Close()
	target.root = nil
	return err
}

func (target *MaildirTarget) prepareBackupDirectory(folder string) (string, *maildirDirectory, error) {
	messagePath, err := target.messagePath(folder)
	if err != nil {
		return "", nil, err
	}
	rel, err := filepath.Rel(target.path, messagePath)
	if err != nil {
		return "", nil, err
	}
	directory, err := target.ensureBackupDirectory(rel)
	if err != nil {
		return "", nil, err
	}
	return rel, directory, nil
}

func (target *MaildirTarget) ensureBackupDirectory(rel string) (*maildirDirectory, error) {
	var created []maildirCreation
	directory, err := target.root.walk(rel, target.create, &created)
	if err == nil {
		err = target.ensureMaildir(directory, &created)
	}
	if err == nil {
		err = target.syncStructure(created)
	}
	if err != nil {
		if directory != nil {
			_ = directory.Close()
		}
		err = errors.Join(err, rollbackMaildirStructure(created))
		closeMaildirCreations(created)
		return nil, err
	}
	closeMaildirCreations(created)
	return directory, nil
}

func (target *MaildirTarget) openBackupDirectories(directory *maildirDirectory, flags []string) (*maildirDirectory, *maildirDirectory, string, string, error) {
	tmp, err := directory.child("tmp", false, nil)
	if err != nil {
		return nil, nil, "", "", err
	}
	flagSuffix := target.maildirFlags(flags)
	newDirName := maildirDirNew
	if flagSuffix != "" {
		newDirName = maildirDirCur
	}
	published, err := directory.child(newDirName, false, nil)
	if err != nil {
		_ = tmp.Close()
		return nil, nil, "", "", err
	}
	return tmp, published, newDirName, flagSuffix, nil
}

func (target *MaildirTarget) createBackupTemp(tmp *maildirDirectory) (*os.File, string, error) {
	file, tmpName, err := tmp.createTemp()
	if err != nil {
		return nil, "", fmt.Errorf("create tmp message: %w", err)
	}
	return file, tmpName, nil
}

func (target *MaildirTarget) cleanupBackupTemp(tmp *maildirDirectory, tmpName string) error {
	remove := target.removeTemp
	if remove == nil {
		remove = removeMaildirFile
	}
	return remove(tmp, tmpName)
}

func (target *MaildirTarget) publishBackupMessage(rel, newDirName string, directory, tmp, published *maildirDirectory, tmpName, flagSuffix string, expected os.FileInfo) (string, error) {
	// Publication uses the pinned tmp and destination directories. Replacing a
	// path entry cannot redirect the hard link or its durability operations.
	link := target.link
	if link == nil {
		link = linkMaildirFile
	}
	newName := tmpName + flagSuffix
	if err := target.linkBackupMessage(directory, tmp, published, tmpName, newName, link); err != nil {
		return "", err
	}
	if err := checkMaildirPublication(published, newName, expected); err != nil {
		return "", errors.Join(err, target.rollbackPublication(published, newName))
	}
	if err := target.syncBackupPublication(directory, tmp, published, newName); err != nil {
		return "", err
	}
	locator := filepath.Join(rel, newDirName, newName)
	target.lastLocator = filepath.ToSlash(locator)
	return locator, nil
}

func (target *MaildirTarget) linkBackupMessage(directory, tmp, published *maildirDirectory, tmpName, newName string, link func(*maildirDirectory, string, *maildirDirectory, string) error) error {
	if err := validateMaildirDirectories(target.root, directory, tmp, published); err != nil {
		return err
	}
	if err := link(tmp, tmpName, published, newName); err != nil {
		return fmt.Errorf("publish maildir message: %w", err)
	}
	return nil
}

func (target *MaildirTarget) syncBackupPublication(directory, tmp, published *maildirDirectory, newName string) error {
	if err := validateMaildirDirectories(target.root, directory, tmp, published); err != nil {
		return errors.Join(err, target.rollbackPublication(published, newName))
	}
	if syncErr := target.syncOpenedDirectory(published); syncErr != nil {
		return errors.Join(fmt.Errorf("sync published maildir message: %w", syncErr), target.rollbackPublication(published, newName))
	}
	if err := validateMaildirDirectories(target.root, directory, tmp, published); err != nil {
		return errors.Join(err, target.rollbackPublication(published, newName))
	}
	return nil
}

func (target *MaildirTarget) messagePath(folder string) (string, error) {
	if !target.preserveFolders || folder == "" {
		return target.path, nil
	}
	parts := strings.Split(folder, "/")
	path := filepath.Join(target.path, ".folders")
	for _, part := range parts {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(part))
		if encoded == "" {
			encoded = "~"
		}
		if len(encoded) > maildirMaxEncodedComponentBytes {
			return "", errors.New("folder component is too long to encode safely")
		}
		path = filepath.Join(path, encoded)
	}
	return path, nil
}

func (target *MaildirTarget) ensureMaildir(directory *maildirDirectory, created *[]maildirCreation) error {
	for _, name := range []string{maildirDirCur, maildirDirNew, "tmp"} {
		child, err := directory.child(name, target.create, created)
		if err != nil {
			return fmt.Errorf("maildir directory %q: %w", name, err)
		}
		_ = child.Close()
	}
	return nil
}

func (target *MaildirTarget) maildirFlags(flags []string) string {
	if !target.preserveFlags {
		return ""
	}
	letters := make(map[rune]bool)
	for _, flag := range flags {
		switch strings.ToLower(flag) {
		case "\\seen":
			letters['S'] = true
		case "\\answered":
			letters['R'] = true
		case "\\flagged":
			letters['F'] = true
		case "\\deleted":
			letters['T'] = true
		case "\\draft":
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
	slices.Sort(ordered)
	var out strings.Builder
	out.WriteString(":2,")
	for _, letter := range ordered {
		out.WriteRune(letter)
	}
	return out.String()
}
