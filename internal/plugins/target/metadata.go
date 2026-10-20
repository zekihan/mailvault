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
func (t *MaildirTarget) UpdateBackupMetadata(ctx context.Context, entry plugins.BackupMessage, flags []string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !t.preserveFlags {
		return entry.Locator, nil
	}
	msg, err := t.ReadBackupMessage(ctx, entry.Locator)
	if err != nil {
		return "", err
	}
	if dedup.ComputeContentHash(msg.Raw) != entry.RawSHA256 {
		return "", fmt.Errorf("backup %q failed checksum verification; repair before refreshing flags", entry.Locator)
	}
	oldPath, err := t.ResolveBackupLocator(entry.Locator)
	if err != nil {
		return "", err
	}
	base := strings.SplitN(filepath.Base(oldPath), ":2,", 2)[0]
	suffix := t.maildirFlags(flags)
	if suffix == "" {
		suffix = ":2,"
	}
	newPath := filepath.Join(filepath.Dir(filepath.Dir(oldPath)), "cur", base+suffix)
	if newPath != oldPath {
		if err := os.Link(oldPath, newPath); err != nil {
			return "", fmt.Errorf("publish updated flags: %w", err)
		}
		if err := t.syncDirectory(filepath.Dir(newPath)); err != nil {
			return "", errors.Join(err, os.Remove(newPath))
		}
		if err := os.Remove(oldPath); err != nil {
			return "", errors.Join(err, os.Remove(newPath))
		}
		if err := t.syncDirectory(filepath.Dir(oldPath)); err != nil {
			return "", err
		}
	}
	locator, err := filepath.Rel(t.path, newPath)
	return filepath.ToSlash(locator), err
}
