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

func cleanupStateDBPath() (string, error) {
	if stateDir != "" {
		return filepath.Join(stateDir, "state.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "mailvault", "state.db"), nil
}

func checkCleanupDBPath(dbPath string) error {
	info, err := os.Stat(dbPath)
	if err != nil {
		return fmt.Errorf("state database %q: %w", dbPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("state database %q is not a regular file", dbPath)
	}
	return nil
}

func runStateCleanup(cmd *cobra.Command, olderThan time.Duration, apply bool) (runErr error) {
	if olderThan <= 0 {
		return errors.New("--older-than must be a positive duration")
	}
	dbPath, err := cleanupStateDBPath()
	if err != nil {
		return err
	}
	if err := checkCleanupDBPath(dbPath); err != nil {
		return err
	}
	ctx := cmd.Context()
	var store *state.Store
	if apply {
		store, err = state.NewCleanupStore(ctx, dbPath)
	} else {
		store, err = state.NewReadOnlyStore(ctx, dbPath)
	}
	if err != nil {
		return fmt.Errorf("open state database: %w", err)
	}
	defer func() { runErr = errors.Join(runErr, store.Close()) }()
	if apply {
		release, err := store.LockSync(ctx)
		if err != nil {
			return fmt.Errorf("lock state database: %w", err)
		}
		defer func() { runErr = errors.Join(runErr, release()) }()
	}
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
}

func newStateCleanupCommand() *cobra.Command {
	var olderThan time.Duration
	var apply bool
	command := &cobra.Command{
		Use:   "cleanup-state",
		Short: "Prune old message metadata from the local state database",
		Long:  "Prune old fetched-message metadata only. Delivery and incremental UID receipts are retained.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStateCleanup(cmd, olderThan, apply)
		},
	}
	command.Flags().DurationVar(&olderThan, "older-than", 0, "minimum age of message metadata to prune (for example 720h)")
	command.Flags().BoolVar(&apply, "apply", false, "delete matching message metadata; by default only show the count")
	return command
}
