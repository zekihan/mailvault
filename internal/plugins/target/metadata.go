package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

// UpdateBackupMetadata changes Maildir filename flags while retaining message bytes.
func (target *MaildirTarget) UpdateBackupMetadata(ctx context.Context, entry *plugins.BackupMessage, flags []string) (string, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !target.preserveFlags {
		return entry.Locator, nil
	}
	root, err := target.backupRoot()
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	file, relative, err := root.resolveMessageBounded(ctx, entry.Locator, target.listingLimits)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	raw, err := readMaildirMessage(file, target.maxMessageBytes)
	if err != nil {
		return "", err
	}
	if dedup.ComputeContentHash(raw) != entry.RawSHA256 {
		return "", fmt.Errorf("backup %q failed checksum verification; repair before refreshing flags", entry.Locator)
	}
	expected, err := file.Stat()
	if err != nil {
		return "", err
	}
	oldDirPath := filepath.Dir(relative)
	oldName := filepath.Base(relative)
	base, _, _ := strings.Cut(oldName, ":2,")
	suffix := target.maildirFlags(flags)
	newRelative, stay := maildirFlagRename(relative, oldDirPath, base, oldName, suffix)
	if stay {
		return filepath.ToSlash(newRelative), nil
	}
	return target.renameMaildirFlags(root, oldDirPath, oldName, newRelative, expected)
}

func maildirFlagRename(relative, oldDirPath, base, oldName, suffix string) (string, bool) {
	if suffix == "" {
		if filepath.Base(oldDirPath) == maildirDirNew && base == oldName {
			return relative, true
		}
		suffix = ":2,"
	}
	newRelative := filepath.Join(filepath.Dir(oldDirPath), maildirDirCur, base+suffix)
	if newRelative == relative {
		return relative, true
	}
	return newRelative, false
}

func (target *MaildirTarget) renameMaildirFlags(root *maildirDirectory, oldDirPath, oldName, newRelative string, expected os.FileInfo) (string, error) {
	newName := filepath.Base(newRelative)
	oldDir, err := root.walk(oldDirPath, false, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = oldDir.Close() }()
	newDir, err := root.walk(filepath.Dir(newRelative), false, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = newDir.Close() }()
	if err := validateMaildirDirectories(root, oldDir, newDir); err != nil {
		return "", err
	}
	if err := linkMaildirFile(oldDir, oldName, newDir, newName); err != nil {
		return "", fmt.Errorf("publish updated flags: %w", err)
	}
	if err := target.syncFlagRename(root, oldDir, oldName, newDir, newName, expected); err != nil {
		return "", err
	}
	return filepath.ToSlash(newRelative), nil
}

func (target *MaildirTarget) syncFlagRename(root, oldDir *maildirDirectory, oldName string, newDir *maildirDirectory, newName string, expected os.FileInfo) error {
	if err := checkMaildirPublication(newDir, newName, expected); err != nil {
		return errors.Join(err, target.rollbackPublication(newDir, newName))
	}
	if err := validateMaildirDirectories(root, oldDir, newDir); err != nil {
		return errors.Join(err, target.rollbackPublication(newDir, newName))
	}
	if err := target.syncOpenedDirectory(newDir); err != nil {
		return errors.Join(err, target.rollbackPublication(newDir, newName))
	}
	if err := oldDir.root.Remove(oldName); err != nil {
		return errors.Join(err, target.rollbackPublication(newDir, newName))
	}
	return target.syncOpenedDirectory(oldDir)
}
