package main

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

func TestSyncHonorsCommandCancellation(t *testing.T) {
	configureCLI(t)
	dryRun = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	if err := runSync(cmd, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("sync ignored cancellation: %v", err)
	}
}
