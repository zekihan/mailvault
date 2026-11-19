package plugins

import (
	"context"
	"errors"
	"testing"
)

type emptyBackupWalker struct {
	cancel context.CancelFunc
}

func (r emptyBackupWalker) WalkBackupMessages(context.Context, func(BackupMessage) error) error {
	r.cancel()
	return nil
}

func (emptyBackupWalker) ListBackupMessages(context.Context) ([]BackupMessage, error) {
	return nil, errors.New("expected streaming backup enumeration")
}

func TestWalkBackupMessagesReportsCancellationAtEndOfStreamingWalk(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := WalkBackupMessages(ctx, emptyBackupWalker{cancel: cancel}, func(BackupMessage) error {
		t.Fatal("empty backup produced a message")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WalkBackupMessages() = %v, want context.Canceled", err)
	}
}
