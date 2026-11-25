package plugins

import (
	"context"
	"errors"
)

var ErrBackupListingLimit = errors.New("backup listing budget exceeded")

// TargetBackupWalker visits entries synchronously, without retaining prior
// batches. Enumeration order is backend-specific. A callback error stops I/O.
type TargetBackupWalker interface {
	WalkBackupMessages(ctx context.Context, visit func(BackupMessage) error) error
}

// WalkBackupMessages supports legacy plugins while preferring incremental readers.
func WalkBackupMessages(ctx context.Context, reader interface {
	ListBackupMessages(ctx context.Context) ([]BackupMessage, error)
}, visit func(BackupMessage) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if walker, ok := reader.(TargetBackupWalker); ok {
		if err := walker.WalkBackupMessages(ctx, visit); err != nil {
			return err
		}
		return ctx.Err()
	}
	entries, err := reader.ListBackupMessages(ctx)
	if err != nil {
		return err
	}
	for i := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(entries[i]); err != nil {
			return err
		}
	}
	return ctx.Err()
}
