package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/zekihan/mailvault/internal/state"
)

func newStateCleanupCommand() *cobra.Command {
	var olderThan time.Duration
	var apply bool
	command := &cobra.Command{
		Use:   "cleanup-state",
		Short: "Prune old message metadata from the local state database",
		Long:  "Prune old fetched-message metadata only. Delivery and incremental UID receipts are retained.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			if olderThan <= 0 {
				return errors.New("--older-than must be a positive duration")
			}
			dir := stateDir
			if dir == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("resolve home directory: %w", err)
				}
				dir = filepath.Join(home, ".local", "share", "mailvault")
			}
			dbPath := filepath.Join(dir, "state.db")
			if info, err := os.Stat(dbPath); err != nil {
				return fmt.Errorf("state database %q: %w", dbPath, err)
			} else if !info.Mode().IsRegular() {
				return fmt.Errorf("state database %q is not a regular file", dbPath)
			}

			var store *state.Store
			var err error
			if apply {
				store, err = state.NewCleanupStore(cmd.Context(), dbPath)
			} else {
				store, err = state.NewReadOnlyStore(dbPath)
			}
			if err != nil {
				return fmt.Errorf("open state database: %w", err)
			}
			defer func() { runErr = errors.Join(runErr, store.Close()) }()
			if apply {
				ctx := cmd.Context()
				release, err := store.LockSync(ctx)
				if err != nil {
					return fmt.Errorf("lock state database: %w", err)
				}
				defer func() { runErr = errors.Join(runErr, release()) }()
			}
			ctx := cmd.Context()
			count, err := store.CountOldMessages(ctx, olderThan)
			if err != nil {
				return fmt.Errorf("count old message metadata: %w", err)
			}
			if apply {
				count, err = store.CleanOldMessages(ctx, olderThan)
				if err != nil {
					return fmt.Errorf("prune old message metadata: %w", err)
				}
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Mode                 string `json:"mode"`
					MessageMetadataCount int64  `json:"message_metadata_count"`
				}{Mode: map[bool]string{true: "applied", false: "preview"}[apply], MessageMetadataCount: count})
			}
			if apply {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed %d old message metadata records; delivery and incremental receipts were retained.\n", count)
			} else {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%d old message metadata records would be removed; use --apply to prune them. Delivery and incremental receipts are retained.\n", count)
			}
			return err
		},
	}
	command.Flags().DurationVar(&olderThan, "older-than", 0, "minimum age of message metadata to prune (for example 720h)")
	command.Flags().BoolVar(&apply, "apply", false, "delete matching message metadata; by default only show the count")
	return command
}
