package plugins

import (
	"context"
	"errors"
)

var ErrBackupListingLimit = errors.New("backup listing budget exceeded")

// TargetBackupWalker visits entries synchronously, without retaining prior
// batches. Enumeration order is backend-specific. A callback error stops I/O.
type TargetBackupWalker interface {
	WalkBackupMessages(context.Context, func(BackupMessage) error) error
}

// WalkBackupMessages supports legacy plugins while preferring incremental readers.
func WalkBackupMessages(ctx context.Context, reader interface {
	ListBackupMessages(context.Context) ([]BackupMessage, error)
}, visit func(BackupMessage) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if walker, ok := reader.(TargetBackupWalker); ok {
		return walker.WalkBackupMessages(ctx, visit)
	}
	entries, err := reader.ListBackupMessages(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(entry); err != nil {
			return err
		}
	}
	return ctx.Err()
}
