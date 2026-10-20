package target

import (
	"context"
	"os"
	"time"
)

// Cooperating writers lock the mailbox inode while appending or rolling back.
func lockMbox(ctx context.Context, file *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		locked, err := tryLockMbox(file)
		if err != nil {
			return err
		}
		if locked {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
