package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/builtin"
	"github.com/zekihan/mailvault/internal/restore"
	"github.com/zekihan/mailvault/internal/state"
)

func init() {
	rootCmd.AddCommand(newRestoreCommand())
}

func newRestoreCommand() *cobra.Command {
	var backupTarget, destinationSource, destinationFolder string
	var preview, allowUnverified bool
	command := &cobra.Command{
		Use:   "restore",
		Short: "Restore messages from a backup target to an IMAP mailbox",
		Long:  "Restore verified backup messages into a separate IMAP mailbox. Existing messages and source backups are never removed.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			if backupTarget == "" {
				return errors.New("--from is required")
			}
			if destinationSource == "" {
				return errors.New("--source is required")
			}
			credentialSource := ""
			if !preview {
				credentialSource = destinationSource
			}
			cfg, err := config.LoadSelected(cfgFile, credentialSource, backupTarget)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			registry := builtin.Registry()
			var targetConfig *config.TargetConfig
			for i := range cfg.Targets {
				if cfg.Targets[i].Name == backupTarget {
					targetConfig = &cfg.Targets[i]
					break
				}
			}
			if targetConfig == nil {
				return fmt.Errorf("backup target %q is not configured", backupTarget)
			}
			var destination *config.SourceConfig
			for i := range cfg.Sources {
				if cfg.Sources[i].Name == destinationSource {
					destination = &cfg.Sources[i]
					break
				}
			}
			if destination == nil {
				return fmt.Errorf("destination source %q is not configured", destinationSource)
			}
			if destination.Type != "imap" {
				return fmt.Errorf("destination source %q must use IMAP", destinationSource)
			}
			backup, err := registry.CreateTarget(targetConfig.Type, targetConfig.Config)
			if err != nil {
				return fmt.Errorf("create backup reader: %w", err)
			}
			defer func() { runErr = errors.Join(runErr, backup.Close()) }()
			if initializer, ok := backup.(interface{ InitializeForRead(context.Context) error }); ok {
				if err := initializer.InitializeForRead(cmd.Context()); err != nil {
					return fmt.Errorf("initialize backup reader: %w", err)
				}
			}
			reader, ok := backup.(plugins.TargetBackupReader)
			if !ok {
				return fmt.Errorf("backup target %q does not support restore reads", backupTarget)
			}
			statePath, err := restoreStatePath()
			if err != nil {
				return err
			}
			var receipts *state.Store
			var manifestStore *state.Store
			if preview {
				if info, err := os.Stat(statePath); err == nil {
					if !info.Mode().IsRegular() {
						return fmt.Errorf("state database %q is not a regular file", statePath)
					}
					manifestStore, err = state.NewReadOnlyStore(statePath)
					if err != nil {
						return fmt.Errorf("open backup manifest: %w", err)
					}
					defer func() { runErr = errors.Join(runErr, manifestStore.Close()) }()
				} else if !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("state database %q: %w", statePath, err)
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(statePath), 0750); err != nil {
					return fmt.Errorf("create state directory: %w", err)
				}
				receipts, err = state.NewStore(statePath)
				if err != nil {
					return fmt.Errorf("open restore receipts: %w", err)
				}
				manifestStore = receipts
				defer func() { runErr = errors.Join(runErr, receipts.Close()) }()
				release, err := receipts.LockSync(cmd.Context())
				if err != nil {
					return fmt.Errorf("lock restore receipts: %w", err)
				}
				defer func() { runErr = errors.Join(runErr, release()) }()
			}
			var manifest []plugins.BackupMessage
			var superseded map[string]bool
			if manifestStore != nil {
				manifest, err = manifestStore.BackupMessages(cmd.Context(), backupTarget)
				if err != nil {
					return fmt.Errorf("read backup manifest: %w", err)
				}
				superseded, err = manifestStore.SupersededBackupObjects(cmd.Context(), backupTarget)
				if err != nil {
					return fmt.Errorf("read superseded backup objects: %w", err)
				}
			}
			reader = restoreTargetReader{TargetBackupReader: reader, target: backupTarget, manifest: manifest, superseded: superseded}

			options := restore.Options{
				BackupTarget: restoreTargetIdentity(*targetConfig), DestinationSource: restoreSourceIdentity(*destination),
				DestinationFolder: destinationFolder, DryRun: preview, AllowUnverified: allowUnverified,
			}
			var appender restore.MailboxAppender
			if !preview {
				pluginConfig, err := config.SourcePluginConfig(*destination)
				if err != nil {
					return fmt.Errorf("configure destination source: %w", err)
				}
				client, err := registry.CreateSource(destination.Type, pluginConfig)
				if err != nil {
					return fmt.Errorf("create destination IMAP client: %w", err)
				}
				defer func() { runErr = errors.Join(runErr, client.Close()) }()
				imapAppender, ok := client.(restore.MailboxAppender)
				if !ok {
					return errors.New("configured IMAP source does not support APPEND")
				}
				appender = imapAppender
			}

			result, err := restore.Run(cmd.Context(), reader, appender, receipts, options)
			if err != nil {
				return err
			}
			if jsonOutput {
				mode := "restored"
				if preview {
					mode = "dry-run"
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Mode              string `json:"mode"`
					BackupTarget      string `json:"backup_target"`
					DestinationSource string `json:"destination_source"`
					DestinationFolder string `json:"destination_folder"`
					Messages          int    `json:"messages"`
					Restored          int    `json:"restored"`
					Planned           int    `json:"planned"`
					Skipped           int    `json:"skipped"`
					Unverified        int    `json:"unverified"`
				}{mode, backupTarget, destinationSource, destinationFolder, result.Messages, result.Restored, result.Planned, result.Skipped, result.Unverified})
			}
			if preview {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Checked %d messages; %d would be restored to %q on %q, with %d lacking a historical manifest hash.\n", result.Messages, result.Planned, destinationFolder, destinationSource, result.Unverified)
			} else {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Restored %d messages to %q on %q; skipped %d completed messages and restored %d legacy messages without historical hashes.\n", result.Restored, destinationFolder, destinationSource, result.Skipped, result.Unverified)
			}
			return err
		},
	}
	command.Flags().StringVar(&backupTarget, "from", "", "configured backup target to read")
	command.Flags().StringVar(&destinationSource, "source", "", "configured IMAP account to restore into")
	command.Flags().StringVar(&destinationFolder, "folder", "", "new destination mailbox for restored messages")
	command.Flags().BoolVar(&preview, "dry-run", false, "verify backup messages without connecting to the destination")
	command.Flags().BoolVar(&allowUnverified, "allow-unverified", false, "allow legacy backup messages without a historical manifest hash")
	_ = command.MarkFlagRequired("folder")
	command.AddCommand(newRestoreResolveCommand())
	return command
}

type restoreTargetReader struct {
	plugins.TargetBackupReader
	target     string
	manifest   []plugins.BackupMessage
	superseded map[string]bool
}

func (r restoreTargetReader) ListBackupMessages(ctx context.Context) ([]plugins.BackupMessage, error) {
	physical, err := r.TargetBackupReader.ListBackupMessages(ctx)
	if err != nil {
		return nil, err
	}
	for i := range physical {
		if physical[i].Target == "" {
			physical[i].Target = r.target
		}
	}
	if len(r.superseded) > 0 {
		for i := range physical {
			if r.isSuperseded(physical[i].Locator) {
				physical[i].Locator = ""
			}
		}
		filtered := physical[:0]
		for _, entry := range physical {
			if entry.Locator != "" {
				filtered = append(filtered, entry)
			}
		}
		physical = filtered
	}
	return restore.MergeBackupMessages(physical, r.manifest)
}

func (r restoreTargetReader) isSuperseded(locator string) bool {
	canonical := func(value string) string {
		if target, ok := r.TargetBackupReader.(interface{ CanonicalBackupLocator(string) string }); ok {
			return target.CanonicalBackupLocator(value)
		}
		if target, ok := r.TargetBackupReader.(interface{ ResolveBackupLocator(string) (string, error) }); ok {
			if resolved, err := target.ResolveBackupLocator(value); err == nil {
				return filepath.Clean(resolved)
			}
		}
		return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(value))), "./")
	}
	key := canonical(locator)
	for old := range r.superseded {
		if key == canonical(old) {
			return true
		}
	}
	return false
}

func restoreStatePath() (string, error) {
	dir := stateDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve state directory: %w", err)
		}
		dir = filepath.Join(home, ".local", "share", "mailvault")
	}
	return filepath.Join(dir, "state.db"), nil
}

func newRestoreResolveCommand() *cobra.Command {
	var backupTarget, destinationSource, destinationFolder, locator, action string
	command := &cobra.Command{
		Use:   "resolve",
		Short: "Resolve uncertain restore state after inspecting the destination",
		Long:  "Resolve a message with an uncertain APPEND outcome or a mailbox setup interrupted before it was recorded active.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			if backupTarget == "" || destinationSource == "" || destinationFolder == "" {
				return errors.New("--from, --source, and --folder are required")
			}
			switch action {
			case "mark-restored", "retry":
				if locator == "" {
					return errors.New("--locator is required for mark-restored and retry")
				}
			case "resume-setup", "retry-setup":
			default:
				return errors.New("--action must be mark-restored, retry, resume-setup, or retry-setup")
			}
			cfg, err := config.LoadSelected(cfgFile, "", backupTarget)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			var targetConfig *config.TargetConfig
			for i := range cfg.Targets {
				if cfg.Targets[i].Name == backupTarget {
					targetConfig = &cfg.Targets[i]
					break
				}
			}
			if targetConfig == nil {
				return fmt.Errorf("backup target %q is not configured", backupTarget)
			}
			var sourceConfig *config.SourceConfig
			for i := range cfg.Sources {
				if cfg.Sources[i].Name == destinationSource {
					sourceConfig = &cfg.Sources[i]
					break
				}
			}
			if sourceConfig == nil || sourceConfig.Type != "imap" {
				return fmt.Errorf("destination source %q is not a configured IMAP account", destinationSource)
			}
			dir := stateDir
			if dir == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("resolve state directory: %w", err)
				}
				dir = filepath.Join(home, ".local", "share", "mailvault")
			}
			dbPath := filepath.Join(dir, "state.db")
			if info, err := os.Stat(dbPath); err != nil {
				return fmt.Errorf("state database %q: %w", dbPath, err)
			} else if !info.Mode().IsRegular() {
				return fmt.Errorf("state database %q is not a regular file", dbPath)
			}
			store, err := state.NewStore(dbPath)
			if err != nil {
				return fmt.Errorf("open restore receipts: %w", err)
			}
			defer func() { runErr = errors.Join(runErr, store.Close()) }()
			release, err := store.LockSync(cmd.Context())
			if err != nil {
				return fmt.Errorf("lock restore receipts: %w", err)
			}
			defer func() { runErr = errors.Join(runErr, release()) }()
			backupID := restoreTargetIdentity(*targetConfig)
			sourceID := restoreSourceIdentity(*sourceConfig)
			if action == "resume-setup" || action == "retry-setup" {
				if err := store.ResolveRestoreJobSetup(cmd.Context(), sourceID, destinationFolder, backupID, action == "resume-setup"); err != nil {
					return err
				}
			} else {
				if err := store.ResolvePendingRestore(cmd.Context(), sourceID, destinationFolder, backupID, locator, action == "mark-restored"); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Resolved uncertain restore state using action %q.\n", action)
			return err
		},
	}
	command.Flags().StringVar(&backupTarget, "from", "", "configured backup target")
	command.Flags().StringVar(&destinationSource, "source", "", "configured destination IMAP account")
	command.Flags().StringVar(&destinationFolder, "folder", "", "destination mailbox")
	command.Flags().StringVar(&locator, "locator", "", "backup message locator shown in the pending restore error")
	command.Flags().StringVar(&action, "action", "", "mark-restored, retry, resume-setup, or retry-setup after inspecting state")
	return command
}

func restoreSourceIdentity(source config.SourceConfig) string {
	values := fmt.Sprintf("%s\x00%s\x00%d\x00%s", source.Name, source.Host, source.Port, source.Username)
	digest := sha256.Sum256([]byte(values))
	return source.Name + ":" + hex.EncodeToString(digest[:])
}

func restoreTargetIdentity(target config.TargetConfig) string {
	fields := []string{target.Name, target.Type}
	for _, key := range []string{"path", "bucket", "region", "endpoint", "prefix"} {
		value, _ := target.Config[key].(string)
		fields = append(fields, value)
	}
	digest := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return target.Name + ":" + hex.EncodeToString(digest[:])
}
