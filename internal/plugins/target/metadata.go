package target

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

// UpdateBackupMetadata changes Maildir filename flags while retaining message bytes.
func (t *MaildirTarget) UpdateBackupMetadata(ctx context.Context, entry plugins.BackupMessage, flags []string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !t.preserveFlags {
		return entry.Locator, nil
	}
	root, err := t.backupRoot()
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	file, relative, err := root.resolveMessage(entry.Locator)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	raw, err := readMaildirMessage(file, maxBackupMessageReadBytes)
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
	oldName := filepath.Base(relative)
	base := strings.SplitN(oldName, ":2,", 2)[0]
	suffix := t.maildirFlags(flags)
	if suffix == "" {
		suffix = ":2,"
	}
	newName := base + suffix
	newRelative := filepath.Join(filepath.Dir(filepath.Dir(relative)), "cur", newName)
	if newRelative == relative {
		return filepath.ToSlash(relative), nil
	}
	oldDir, err := root.walk(filepath.Dir(relative), false, nil)
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
	if err := checkMaildirPublication(newDir, newName, expected); err != nil {
		return "", errors.Join(err, t.rollbackPublication(newDir, newName))
	}
	if err := validateMaildirDirectories(root, oldDir, newDir); err != nil {
		return "", errors.Join(err, t.rollbackPublication(newDir, newName))
	}
	if err := t.syncOpenedDirectory(newDir); err != nil {
		return "", errors.Join(err, t.rollbackPublication(newDir, newName))
	}
	if err := oldDir.root.Remove(oldName); err != nil {
		return "", errors.Join(err, t.rollbackPublication(newDir, newName))
	}
	if err := t.syncOpenedDirectory(oldDir); err != nil {
		return "", err
	}
	return filepath.ToSlash(newRelative), nil
}
