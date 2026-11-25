package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins/builtin"
	"github.com/zekihan/mailvault/internal/state"
	"github.com/zekihan/mailvault/internal/sync"
)

// Set by the release build's -X main.version linker flag.
var version = "dev"

const (
	defaultSyncConcurrency = 5
	defaultMaxRetries      = 3
)

const stateDirPerms = 0o750

var (
	cfgFile       string
	dryRun        bool
	forceFetchAll bool
	refreshFlags  bool
	jsonOutput    bool
	since         string
	until         string
	concurrency   int
	maxRetries    int
	retryDelay    string
	stateDir      string
	repairMode    bool
	repairSource  string
	repairTarget  string
)

var rootCmd = &cobra.Command{
	Use:   "mailvault",
	Short: "Back up mail from multiple sources to multiple targets",
	Long: `mailvault backs up mail from multiple sources (IMAP, POP3) to multiple targets (Maildir, mbox, S3).

Configuration is read from a YAML file specifying sources, targets, and source-target pairs.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		return nil
	},
}

func Execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return rootCmd.ExecuteContext(ctx)
}

func init() {
	rootCmd.AddCommand(newStateCleanupCommand())
	rootCmd.AddCommand(newVerifyCommand())
	repairCmd := &cobra.Command{Use: "repair", Short: "Repair a source-to-target backup after verification", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		apply, err := cmd.Flags().GetBool("apply")
		if err != nil {
			return fmt.Errorf("read apply flag: %w", err)
		}
		if !apply {
			return errors.New("repair writes backup data; pass --apply to continue")
		}
		if repairSource == "" || repairTarget == "" {
			return errors.New("--source and --target are required")
		}
		repairMode = true
		defer func() { repairMode = false }()
		return runSync(cmd, nil)
	}}
	repairCmd.Flags().StringVar(&repairSource, "source", "", "configured source to repair from")
	repairCmd.Flags().StringVar(&repairTarget, "target", "", "configured target to repair")
	repairCmd.Flags().Bool("apply", false, "write repaired copies after checking the target")
	rootCmd.AddCommand(repairCmd)
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: ./config.yaml, ~/.config/mailvault/config.yaml, /etc/mailvault/config.yaml)")
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "output JSON instead of human-readable format")
	rootCmd.PersistentFlags().StringVar(&stateDir, "state-dir", "", "directory for state database (default: ~/.local/share/mailvault)")

	// Sync command.
	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Synchronize mail from sources to targets",
		RunE:  runSync,
	}
	syncCmd.Flags().BoolVar(&forceFetchAll, "force-fetch-all", false, "download all selected messages again, ignoring incremental receipts")
	syncCmd.Flags().BoolVar(&refreshFlags, "refresh-flags", false, "refresh saved IMAP flags without downloading existing message bodies")
	syncCmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be done without making changes")
	syncCmd.Flags().StringVar(&since, "since", "", "only fetch messages at or after this timestamp (RFC3339, inclusive)")
	syncCmd.Flags().StringVar(&until, "until", "", "only fetch messages before this timestamp (RFC3339, exclusive)")
	syncCmd.Flags().IntVar(&concurrency, "concurrency", defaultSyncConcurrency, "maximum concurrent source-target pairs")
	syncCmd.Flags().IntVar(&maxRetries, "max-retries", defaultMaxRetries, "maximum retries for failed writes")
	syncCmd.Flags().StringVar(&retryDelay, "retry-delay", "1s", "base delay between retries")
	rootCmd.AddCommand(syncCmd)

	// Validate-config command.
	validateCmd := &cobra.Command{
		Use:   "validate-config",
		Short: "Validate configuration file and test connectivity",
		RunE:  runValidateConfig,
	}
	rootCmd.AddCommand(validateCmd)

	// List-sources command.
	listSourcesCmd := &cobra.Command{
		Use:   "list-sources",
		Short: "List configured sources",
		RunE:  runListSources,
	}
	rootCmd.AddCommand(listSourcesCmd)

	// List-targets command.
	listTargetsCmd := &cobra.Command{
		Use:   "list-targets",
		Short: "List configured targets",
		RunE:  runListTargets,
	}
	rootCmd.AddCommand(listTargetsCmd)

	// Version command.
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run:   runVersion,
	}
	rootCmd.AddCommand(versionCmd)
}

func commandContext(cmd *cobra.Command) context.Context {
	if cmd != nil && cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}

func loadConfig() (*config.Config, error) {
	return config.Load(cfgFile)
}

func loadStateStore(ctx context.Context) (*state.Store, error) {
	dir := stateDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("expand home directory: %w", err)
		}
		dir = home + "/.local/share/mailvault"
	}
	dbPath := dir + "/state.db"
	if dryRun {
		if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
			return state.NewStore(ctx, ":memory:")
		} else if err != nil {
			return nil, fmt.Errorf("stat state database: %w", err)
		}
		return state.NewReadOnlyStore(ctx, dbPath)
	}
	if err := os.MkdirAll(dir, stateDirPerms); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return state.NewStore(ctx, dbPath)
}

func loadSyncConfig() (*config.Config, error) {
	if repairMode {
		return config.LoadSelected(cfgFile, repairSource, repairTarget)
	}
	return loadConfig()
}

func repairPairConfigured(cfg *config.Config) bool {
	for _, pair := range cfg.Pairs {
		if pair.Source == repairSource && containsString(pair.Targets, repairTarget) {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func applyRepairFilter(cfg *config.Config) error {
	if !repairMode {
		return nil
	}
	if !repairPairConfigured(cfg) {
		return fmt.Errorf("configured source-target pair %q -> %q not found", repairSource, repairTarget)
	}
	cfg.Pairs = []config.PairConfig{{Source: repairSource, Targets: []string{repairTarget}}}
	return nil
}

func parseSyncOptions() (time.Duration, int64, int64, error) {
	retryDelayDur, err := time.ParseDuration(retryDelay)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse retry-delay: %w", err)
	}
	sinceTime, err := parseTimeBound(since)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse since: %w", err)
	}
	untilTime, err := parseTimeBound(until)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse until: %w", err)
	}
	return retryDelayDur, sinceTime, untilTime, nil
}

func newSyncEngine(cfg *config.Config, stateStore *state.Store, progress func(string, string), retryDelayDur time.Duration, sinceTime, untilTime int64) (*sync.SyncEngine, error) {
	engine, err := sync.NewSyncEngine(cfg, stateStore, builtin.Registry(),
		sync.WithDryRun(dryRun),
		sync.WithForceFetchAll(forceFetchAll),
		sync.WithRepair(repairMode),
		sync.WithProgress(progress),
		sync.WithConcurrency(concurrency),
		sync.WithMaxRetries(maxRetries),
		sync.WithRetryDelay(retryDelayDur),
		sync.WithRefreshFlags(refreshFlags),
		sync.WithSince(sinceTime),
		sync.WithUntil(untilTime),
	)
	if err != nil {
		return nil, fmt.Errorf("create sync engine: %w", err)
	}
	return engine, nil
}

func runSync(cmd *cobra.Command, args []string) (err error) {
	ctx := commandContext(cmd)
	cfg, err := loadSyncConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	if err := applyRepairFilter(cfg); err != nil {
		return err
	}
	stateStore, err := loadStateStore(ctx)
	if err != nil {
		return fmt.Errorf("load state store: %w", err)
	}
	defer func() {
		err = errors.Join(err, stateStore.Close())
	}()
	retryDelayDur, sinceTime, untilTime, err := parseSyncOptions()
	if err != nil {
		return err
	}
	progress, stopProgress := startSyncProgress(os.Stderr, dryRun)
	defer stopProgress()
	engine, err := newSyncEngine(cfg, stateStore, progress, retryDelayDur, sinceTime, untilTime)
	if err != nil {
		return err
	}
	stats, err := engine.Run(ctx)
	if err != nil {
		syncErr := fmt.Errorf("sync failed: %w", err)
		if stats != nil && jsonOutput {
			return errors.Join(syncErr, printStats(stats))
		}
		return syncErr
	}
	if err := printStats(stats); err != nil {
		return err
	}
	if stats.TotalErrors > 0 {
		return fmt.Errorf("sync completed with %d errors", stats.TotalErrors)
	}
	return nil
}

func printStats(stats *sync.SyncStats) error {
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(stats)
	}

	fmt.Printf("Sync completed in %v\n", stats.Duration.Round(time.Millisecond))
	fmt.Printf("Sources: %d, Targets: %d, Pairs: %d\n", stats.TotalSources, stats.TotalTargets, stats.TotalPairs)
	fmt.Printf("Messages: %d fetched, %d written, %d skipped, %d errors\n",
		stats.TotalFetched, stats.TotalWritten, stats.TotalSkipped, stats.TotalErrors)

	for _, result := range stats.Results {
		status := "OK"
		if len(result.Errors) > 0 {
			status = "ERROR"
		}
		fmt.Printf("  %s -> %s: %s (%d fetched, %d written, %d skipped, %d errors, %v)\n",
			result.SourceName, result.TargetName, status,
			result.MessagesFetched, result.MessagesWritten, result.MessagesSkipped,
			len(result.Errors), result.Duration.Round(time.Millisecond))
		for _, err := range result.Errors {
			fmt.Printf("    ERROR: %v\n", err)
		}
	}
	for _, message := range stats.FinalizationErrors {
		fmt.Printf("  ERROR: %s\n", message)
	}
	return nil
}

func runValidateConfig(cmd *cobra.Command, args []string) error {
	report, validationErr := validateConfiguration(commandContext(cmd))
	return errors.Join(validationErr, printValidationReport(&report))
}

func runListSources(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg = listingConfig(cfg)

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cfg.Sources)
	}

	for i := range cfg.Sources {
		src := &cfg.Sources[i]
		fmt.Printf("%s (%s://%s:%d)\n", src.Name, src.Type, src.Host, src.Port)
		fmt.Printf("  Username: %s\n", src.Username)
		fmt.Printf("  Folders: %s\n", strings.Join(src.Folders, ", "))
		fmt.Printf("  TLS: %v, StartTLS: %v\n", src.UseTLS, src.StartTLS)
	}
	return nil
}

func runListTargets(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg = listingConfig(cfg)

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cfg.Targets)
	}

	for _, tgt := range cfg.Targets {
		fmt.Printf("%s (%s)\n", tgt.Name, tgt.Type)
		for k, v := range tgt.Config {
			fmt.Printf("  %s: %v\n", k, v)
		}
	}
	return nil
}

func runVersion(cmd *cobra.Command, args []string) {
	fmt.Printf("mailvault version %s\n", version)
}

func main() {
	if err := Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
