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

func loadRestoreConfig(backupTarget, destinationSource string, preview bool) (*config.Config, error) {
	credentialSource := ""
	if !preview {
		credentialSource = destinationSource
	}
	cfg, err := config.LoadSelected(cfgFile, credentialSource, backupTarget)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return cfg, nil
}

func findRestoreTarget(cfg *config.Config, backupTarget string) *config.TargetConfig {
	for i := range cfg.Targets {
		if cfg.Targets[i].Name == backupTarget {
			return &cfg.Targets[i]
		}
	}
	return nil
}

func findRestoreSource(cfg *config.Config, destinationSource string) *config.SourceConfig {
	for i := range cfg.Sources {
		if cfg.Sources[i].Name == destinationSource {
			return &cfg.Sources[i]
		}
	}
	return nil
}

func resolveRestoreConfigs(cfg *config.Config, backupTarget, destinationSource string) (*config.TargetConfig, *config.SourceConfig, error) {
	targetConfig := findRestoreTarget(cfg, backupTarget)
	if targetConfig == nil {
		return nil, nil, fmt.Errorf("backup target %q is not configured", backupTarget)
	}
	destination := findRestoreSource(cfg, destinationSource)
	if destination == nil {
		return nil, nil, fmt.Errorf("destination source %q is not configured", destinationSource)
	}
	if destination.Type != "imap" {
		return nil, nil, fmt.Errorf("destination source %q must use IMAP", destinationSource)
	}
	return targetConfig, destination, nil
}

func openRestoreBackup(ctx context.Context, cfg *config.Config, targetConfig *config.TargetConfig, backupTarget string) (plugins.Target, plugins.TargetBackupReader, string, error) {
	backup, err := builtin.Registry().CreateTarget(targetConfig.Type, config.TargetPluginConfig(*targetConfig, cfg.MaxMessageBytes))
	if err != nil {
		return nil, nil, "", fmt.Errorf("create backup reader: %w", err)
	}
	backupStateKey := plugins.TargetStateKey(backup)
	if initializer, ok := backup.(interface {
		InitializeForRead(ctx context.Context) error
	}); ok {
		if err := initializer.InitializeForRead(ctx); err != nil {
			return nil, nil, "", fmt.Errorf("initialize backup reader: %w", err)
		}
	}
	reader, ok := backup.(plugins.TargetBackupReader)
	if !ok {
		return nil, nil, "", fmt.Errorf("backup target %q does not support restore reads", backupTarget)
	}
	return backup, reader, backupStateKey, nil
}

var errPreviewRestoreStoreAbsent = errors.New("preview restore store is absent")

func openPreviewRestoreStore(ctx context.Context, statePath string) (*state.Store, error) {
	info, err := os.Stat(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errPreviewRestoreStoreAbsent
		}
		return nil, fmt.Errorf("state database %q: %w", statePath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("state database %q is not a regular file", statePath)
	}
	manifestStore, err := state.NewReadOnlyStore(ctx, statePath)
	if err != nil {
		return nil, fmt.Errorf("open backup manifest: %w", err)
	}
	return manifestStore, nil
}

func openWriteRestoreStore(ctx context.Context, statePath string) (*state.Store, func() error, error) {
	if err := os.MkdirAll(filepath.Dir(statePath), stateDirPerms); err != nil {
		return nil, nil, fmt.Errorf("create state directory: %w", err)
	}
	receipts, err := state.NewStore(ctx, statePath)
	if err != nil {
		return nil, nil, fmt.Errorf("open restore receipts: %w", err)
	}
	release, err := receipts.LockSync(ctx)
	if err != nil {
		return receipts, nil, fmt.Errorf("lock restore receipts: %w", err)
	}
	return receipts, release, nil
}

func loadRestoreManifest(ctx context.Context, manifestStore *state.Store, backupStateKey string) ([]plugins.BackupMessage, map[string]bool, error) {
	if manifestStore == nil {
		return nil, nil, nil
	}
	manifest, err := manifestStore.BackupMessages(ctx, backupStateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("read backup manifest: %w", err)
	}
	superseded, err := manifestStore.SupersededBackupObjects(ctx, backupStateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("read superseded backup objects: %w", err)
	}
	return manifest, superseded, nil
}

func openRestoreAppenderClient(cfg *config.Config, destination *config.SourceConfig) (plugins.Source, error) {
	pluginConfig, err := config.SourcePluginConfig(destination, cfg.MaxMessageBytes)
	if err != nil {
		return nil, fmt.Errorf("configure destination source: %w", err)
	}
	client, err := builtin.Registry().CreateSource(destination.Type, pluginConfig)
	if err != nil {
		return nil, fmt.Errorf("create destination IMAP client: %w", err)
	}
	return client, nil
}

func asMailboxAppender(client plugins.Source) (restore.MailboxAppender, error) {
	imapAppender, ok := client.(restore.MailboxAppender)
	if !ok {
		return nil, errors.New("configured IMAP source does not support APPEND")
	}
	return imapAppender, nil
}

type restoreResult struct {
	Messages   int
	Restored   int
	Planned    int
	Skipped    int
	Unverified int
}

func printRestoreResult(cmd *cobra.Command, preview bool, backupTarget, destinationSource, destinationFolder string, result restoreResult) error {
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
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Checked %d messages; %d would be restored to %q on %q, with %d lacking a historical manifest hash.\n", result.Messages, result.Planned, destinationFolder, destinationSource, result.Unverified)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "Restored %d messages to %q on %q; skipped %d completed messages and restored %d legacy messages without historical hashes.\n", result.Restored, destinationFolder, destinationSource, result.Skipped, result.Unverified)
	return err
}

func openRestoreStateStores(ctx context.Context, statePath string, preview bool) (*state.Store, *state.Store, func() error, error) {
	cleanup := func() error { return nil }
	if preview {
		manifestStore, err := openPreviewRestoreStore(ctx, statePath)
		if errors.Is(err, errPreviewRestoreStoreAbsent) {
			manifestStore, err = nil, nil
		}
		if err != nil {
			return nil, nil, func() error { return nil }, err
		}
		if manifestStore != nil {
			cleanup = func() error { return manifestStore.Close() }
		}
		return nil, manifestStore, cleanup, nil
	}
	receipts, release, err := openWriteRestoreStore(ctx, statePath)
	if err != nil {
		if receipts != nil {
			cleanup = func() error { return receipts.Close() }
		}
		return receipts, nil, cleanup, err
	}
	cleanup = func() error { return errors.Join(release(), receipts.Close()) }
	return receipts, receipts, cleanup, nil
}

func openRestoreAppenderIfNeeded(cfg *config.Config, destination *config.SourceConfig, preview bool) (restore.MailboxAppender, func() error, error) {
	cleanup := func() error { return nil }
	if preview {
		return nil, cleanup, nil
	}
	client, err := openRestoreAppenderClient(cfg, destination)
	if err != nil {
		return nil, nil, err
	}
	appender, err := asMailboxAppender(client)
	if err != nil {
		return nil, func() error { return client.Close() }, err
	}
	return appender, func() error { return client.Close() }, nil
}

func runRestore(cmd *cobra.Command, backupTarget, destinationSource, destinationFolder string, preview, allowUnverified bool) (runErr error) {
	if backupTarget == "" {
		return errors.New("--from is required")
	}
	if destinationSource == "" {
		return errors.New("--source is required")
	}
	cfg, err := loadRestoreConfig(backupTarget, destinationSource, preview)
	if err != nil {
		return err
	}
	targetConfig, destination, err := resolveRestoreConfigs(cfg, backupTarget, destinationSource)
	if err != nil {
		return err
	}
	backup, reader, backupStateKey, err := openRestoreBackup(cmd.Context(), cfg, targetConfig, backupTarget)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, backup.Close()) }()
	statePath, err := restoreStatePath()
	if err != nil {
		return err
	}
	receipts, manifestStore, cleanup, err := openRestoreStateStores(cmd.Context(), statePath, preview)
	defer func() { runErr = errors.Join(runErr, cleanup()) }()
	if err != nil {
		return err
	}
	manifest, superseded, err := loadRestoreManifest(cmd.Context(), manifestStore, backupStateKey)
	if err != nil {
		return err
	}
	wrapped := restoreTargetReader{TargetBackupReader: reader, target: backupStateKey, manifest: manifest, superseded: superseded}
	options := restore.Options{
		BackupTarget: backupStateKey, DestinationSource: restoreSourceIdentity(destination),
		DestinationFolder: destinationFolder, DryRun: preview, AllowUnverified: allowUnverified,
	}
	appender, closeAppender, err := openRestoreAppenderIfNeeded(cfg, destination, preview)
	if closeAppender != nil {
		defer func() { runErr = errors.Join(runErr, closeAppender()) }()
	}
	if err != nil {
		return err
	}
	result, err := restore.Run(cmd.Context(), wrapped, appender, receipts, options)
	if err != nil {
		return err
	}
	return printRestoreResult(cmd, preview, backupTarget, destinationSource, destinationFolder, restoreResult{Messages: result.Messages, Restored: result.Restored, Planned: result.Planned, Skipped: result.Skipped, Unverified: result.Unverified})
}

func newRestoreCommand() *cobra.Command {
	var backupTarget, destinationSource, destinationFolder string
	var preview, allowUnverified bool
	command := &cobra.Command{
		Use:   "restore",
		Short: "Restore messages from a backup target to an IMAP mailbox",
		Long:  "Restore verified backup messages into a separate IMAP mailbox. Existing messages and source backups are never removed.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestore(cmd, backupTarget, destinationSource, destinationFolder, preview, allowUnverified)
		},
	}
	command.Flags().StringVar(&backupTarget, "from", "", "configured backup target to read")
	command.Flags().StringVar(&destinationSource, "source", "", "configured IMAP account to restore into")
	command.Flags().StringVar(&destinationFolder, "folder", "", "new destination mailbox for restored messages")
	command.Flags().BoolVar(&preview, "dry-run", false, "verify backup messages without connecting to the destination")
	command.Flags().BoolVar(&allowUnverified, "allow-unverified", false, "allow legacy backup messages without a historical manifest hash")
	_ = command.MarkFlagRequired("folder") //nolint:errcheck // flag defined above; registration cannot fail
	command.AddCommand(newRestoreResolveCommand())
	return command
}

type restoreTargetReader struct {
	plugins.TargetBackupReader

	target     string
	manifest   []plugins.BackupMessage
	superseded map[string]bool
}

func (targetReader restoreTargetReader) ListBackupMessages(ctx context.Context) ([]plugins.BackupMessage, error) {
	var result []plugins.BackupMessage
	err := targetReader.WalkBackupMessages(ctx, func(entry plugins.BackupMessage) error { result = append(result, entry); return nil })
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (targetReader restoreTargetReader) WalkBackupMessages(ctx context.Context, visit func(plugins.BackupMessage) error) error {
	manifest := make(map[string][]plugins.BackupMessage)
	for i := range targetReader.manifest {
		entry := &targetReader.manifest[i]
		key := targetReader.canonicalLocator(entry.Locator)
		manifest[key] = append(manifest[key], *entry)
	}
	retired := make(map[string]bool)
	for locator := range targetReader.superseded {
		retired[targetReader.canonicalLocator(locator)] = true
	}
	for key := range manifest {
		delete(retired, key)
	}
	seen := make(map[string]bool)
	err := plugins.WalkBackupMessages(ctx, targetReader.TargetBackupReader, func(entry plugins.BackupMessage) error {
		key := targetReader.canonicalLocator(entry.Locator)
		if retired[key] || seen[key] {
			return nil
		}
		if entry.Target == "" {
			entry.Target = targetReader.target
		}
		merged, err := restore.MergeBackupMessagesCanonical([]plugins.BackupMessage{entry}, manifest[key], targetReader.canonicalLocator)
		if err != nil {
			return err
		}
		seen[key] = true
		return visit(merged[0])
	})
	if err != nil {
		return err
	}
	for key, entries := range manifest {
		if !seen[key] {
			return fmt.Errorf("manifest locator %q has no physical backup message: %w", entries[0].Locator, plugins.ErrBackupMessageMissing)
		}
	}
	return ctx.Err()
}

func (targetReader restoreTargetReader) canonicalLocator(value string) string {
	if target, ok := targetReader.TargetBackupReader.(interface{ CanonicalBackupLocator(locator string) string }); ok {
		return target.CanonicalBackupLocator(value)
	}
	if target, ok := targetReader.TargetBackupReader.(interface {
		ResolveBackupLocator(locator string) (string, error)
	}); ok {
		if resolved, err := target.ResolveBackupLocator(value); err == nil {
			return filepath.Clean(resolved)
		}
	}
	return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(value))), "./")
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

func validateRestoreResolveArgs(backupTarget, destinationSource, destinationFolder, locator, action string) error {
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
	return nil
}

func openResolveBackupTarget(cfg *config.Config, backupTarget string) (plugins.Target, *config.TargetConfig, error) {
	targetConfig := findRestoreTarget(cfg, backupTarget)
	if targetConfig == nil {
		return nil, nil, fmt.Errorf("backup target %q is not configured", backupTarget)
	}
	backup, err := builtin.Registry().CreateTarget(targetConfig.Type, config.TargetPluginConfig(*targetConfig, cfg.MaxMessageBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("create backup target: %w", err)
	}
	return backup, targetConfig, nil
}

func resolveRestoreSourceConfig(cfg *config.Config, destinationSource string) (*config.SourceConfig, error) {
	sourceConfig := findRestoreSource(cfg, destinationSource)
	if sourceConfig == nil || sourceConfig.Type != "imap" {
		return nil, fmt.Errorf("destination source %q is not a configured IMAP account", destinationSource)
	}
	return sourceConfig, nil
}

func openResolveStateStore(ctx context.Context) (*state.Store, func() error, error) {
	dir := stateDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, fmt.Errorf("resolve state directory: %w", err)
		}
		dir = filepath.Join(home, ".local", "share", "mailvault")
	}
	dbPath := filepath.Join(dir, "state.db")
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("state database %q: %w", dbPath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("state database %q is not a regular file", dbPath)
	}
	store, err := state.NewStore(ctx, dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open restore receipts: %w", err)
	}
	release, err := store.LockSync(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("lock restore receipts: %w", err)
	}
	return store, release, nil
}

func runRestoreResolve(cmd *cobra.Command, backupTarget, destinationSource, destinationFolder, locator, action string) (runErr error) {
	if err := validateRestoreResolveArgs(backupTarget, destinationSource, destinationFolder, locator, action); err != nil {
		return err
	}
	cfg, err := config.LoadSelected(cfgFile, "", backupTarget)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	backup, _, err := openResolveBackupTarget(cfg, backupTarget)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, backup.Close()) }()
	sourceConfig, err := resolveRestoreSourceConfig(cfg, destinationSource)
	if err != nil {
		return err
	}
	store, release, err := openResolveStateStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, store.Close()) }()
	defer func() { runErr = errors.Join(runErr, release()) }()
	backupID := plugins.TargetStateKey(backup)
	sourceID := restoreSourceIdentity(sourceConfig)
	if action == "resume-setup" || action == "retry-setup" {
		if err := store.ResolveRestoreJobSetup(cmd.Context(), sourceID, destinationFolder, backupID, action == "resume-setup"); err != nil {
			return err
		}
	} else if err := store.ResolvePendingRestore(cmd.Context(), sourceID, destinationFolder, backupID, locator, action == "mark-restored"); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Resolved uncertain restore state using action %q.\n", action)
	return err
}

func newRestoreResolveCommand() *cobra.Command {
	var backupTarget, destinationSource, destinationFolder, locator, action string
	command := &cobra.Command{
		Use:   "resolve",
		Short: "Resolve uncertain restore state after inspecting the destination",
		Long:  "Resolve a message with an uncertain APPEND outcome or a mailbox setup interrupted before it was recorded active.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestoreResolve(cmd, backupTarget, destinationSource, destinationFolder, locator, action)
		},
	}
	command.Flags().StringVar(&backupTarget, "from", "", "configured backup target")
	command.Flags().StringVar(&destinationSource, "source", "", "configured destination IMAP account")
	command.Flags().StringVar(&destinationFolder, "folder", "", "destination mailbox")
	command.Flags().StringVar(&locator, "locator", "", "backup message locator shown in the pending restore error")
	command.Flags().StringVar(&action, "action", "", "mark-restored, retry, resume-setup, or retry-setup after inspecting state")
	return command
}

func restoreSourceIdentity(source *config.SourceConfig) string {
	values := fmt.Sprintf("%s\x00%s\x00%d\x00%s", source.Name, source.Host, source.Port, source.Username)
	digest := sha256.Sum256([]byte(values))
	return source.Name + ":" + hex.EncodeToString(digest[:])
}
