package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins/builtin"
	"github.com/zekihan/mailvault/internal/state"
	"github.com/zekihan/mailvault/internal/sync"
)

var (
	cfgFile     string
	dryRun      bool
	jsonOutput  bool
	since       string
	until       string
	concurrency int
	maxRetries  int
	retryDelay  string
	stateDir    string
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
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: ./config.yaml, ~/.config/mailvault/config.yaml, /etc/mailvault/config.yaml)")
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "output JSON instead of human-readable format")
	rootCmd.PersistentFlags().StringVar(&stateDir, "state-dir", "", "directory for state database (default: ~/.local/share/mailvault)")

	// Sync command
	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Synchronize mail from sources to targets",
		RunE:  runSync,
	}
	syncCmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be done without making changes")
	syncCmd.Flags().StringVar(&since, "since", "", "only fetch messages after this date (RFC3339)")
	syncCmd.Flags().StringVar(&until, "until", "", "only fetch messages before this date (RFC3339)")
	syncCmd.Flags().IntVar(&concurrency, "concurrency", 5, "maximum concurrent source-target pairs")
	syncCmd.Flags().IntVar(&maxRetries, "max-retries", 3, "maximum retries for failed writes")
	syncCmd.Flags().StringVar(&retryDelay, "retry-delay", "1s", "base delay between retries")
	rootCmd.AddCommand(syncCmd)

	// Validate-config command
	validateCmd := &cobra.Command{
		Use:   "validate-config",
		Short: "Validate configuration file and test connectivity",
		RunE:  runValidateConfig,
	}
	rootCmd.AddCommand(validateCmd)

	// List-sources command
	listSourcesCmd := &cobra.Command{
		Use:   "list-sources",
		Short: "List configured sources",
		RunE:  runListSources,
	}
	rootCmd.AddCommand(listSourcesCmd)

	// List-targets command
	listTargetsCmd := &cobra.Command{
		Use:   "list-targets",
		Short: "List configured targets",
		RunE:  runListTargets,
	}
	rootCmd.AddCommand(listTargetsCmd)

	// Version command
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run:   runVersion,
	}
	rootCmd.AddCommand(versionCmd)
}

func initConfig() {
	// Config loading is handled in each command
}

func loadConfig() (*config.Config, error) {
	return config.Load(cfgFile)
}

func loadStateStore() (*state.Store, error) {
	dir := stateDir
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = fmt.Sprintf("%s/.local/share/mailvault", home)
	}
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	dbPath := fmt.Sprintf("%s/state.db", dir)
	return state.NewStore(dbPath)
}

func runSync(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	stateStore, err := loadStateStore()
	if err != nil {
		return fmt.Errorf("load state store: %w", err)
	}
	defer stateStore.Close()

	registry := builtin.Registry()

	retryDelayDur, err := time.ParseDuration(retryDelay)
	if err != nil {
		return fmt.Errorf("parse retry-delay: %w", err)
	}

	var sinceTime, untilTime int64
	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return fmt.Errorf("parse since: %w", err)
		}
		sinceTime = t.Unix()
	}
	if until != "" {
		t, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return fmt.Errorf("parse until: %w", err)
		}
		untilTime = t.Unix()
	}

	engine, err := sync.NewSyncEngine(cfg, stateStore, registry,
		sync.WithDryRun(dryRun),
		sync.WithConcurrency(concurrency),
		sync.WithMaxRetries(maxRetries),
		sync.WithRetryDelay(retryDelayDur),
		sync.WithSince(sinceTime),
		sync.WithUntil(untilTime),
	)
	if err != nil {
		return fmt.Errorf("create sync engine: %w", err)
	}

	stats, err := engine.Run(ctx)
	if err != nil {
		return fmt.Errorf("sync failed: %w", err)
	}

	printStats(stats)
	return nil
}

func printStats(stats *sync.SyncStats) {
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(stats)
		return
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
}

func runValidateConfig(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	fmt.Println("Configuration is valid")
	fmt.Printf("Sources: %d\n", len(cfg.Sources))
	for _, src := range cfg.Sources {
		fmt.Printf("  - %s (%s://%s:%d)\n", src.Name, src.Type, src.Host, src.Port)
	}
	fmt.Printf("Targets: %d\n", len(cfg.Targets))
	for _, tgt := range cfg.Targets {
		fmt.Printf("  - %s (%s)\n", tgt.Name, tgt.Type)
	}
	fmt.Printf("Pairs: %d\n", len(cfg.Pairs))
	for _, pair := range cfg.Pairs {
		fmt.Printf("  - %s -> %s\n", pair.Source, strings.Join(pair.Targets, ", "))
	}

	// Test connectivity if not dry-run
	if !dryRun {
		fmt.Println("\nTesting connectivity...")
		registry := builtin.Registry()
		stateStore, err := loadStateStore()
		if err != nil {
			return fmt.Errorf("load state store: %w", err)
		}
		defer stateStore.Close()

		ctx := context.Background()

		// Test sources
		for _, srcCfg := range cfg.Sources {
			srcConfig := map[string]interface{}{
				"host":               srcCfg.Host,
				"port":               srcCfg.Port,
				"username":           srcCfg.Username,
				"password_ref":       srcCfg.PasswordRef,
				"use_tls":            srcCfg.UseTLS,
				"start_tls":          srcCfg.StartTLS,
				"folders":            srcCfg.Folders,
				"connection_timeout": srcCfg.ConnectionTimeout,
				"read_timeout":       srcCfg.ReadTimeout,
				"max_connections":    srcCfg.MaxConnections,
				"name":               srcCfg.Name,
			}
			src, err := registry.CreateSource(srcCfg.Type, srcConfig)
			if err != nil {
				fmt.Printf("  Source %s: FAILED - %v\n", srcCfg.Name, err)
				continue
			}
			if err := src.Connect(ctx); err != nil {
				fmt.Printf("  Source %s: FAILED - %v\n", srcCfg.Name, err)
			} else {
				fmt.Printf("  Source %s: OK\n", srcCfg.Name)
				src.Close()
			}
		}

		// Test targets
		for _, tgtCfg := range cfg.Targets {
			tgtConfig := tgtCfg.Config
			if tgtConfig == nil {
				tgtConfig = make(map[string]interface{})
			}
			tgtConfig["name"] = tgtCfg.Name
			tgt, err := registry.CreateTarget(tgtCfg.Type, tgtConfig)
			if err != nil {
				fmt.Printf("  Target %s: FAILED - %v\n", tgtCfg.Name, err)
				continue
			}
			if err := tgt.Initialize(ctx); err != nil {
				fmt.Printf("  Target %s: FAILED - %v\n", tgtCfg.Name, err)
			} else {
				fmt.Printf("  Target %s: OK\n", tgtCfg.Name)
				tgt.Close()
			}
		}
	}

	return nil
}

func runListSources(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(cfg.Sources)
		return nil
	}

	for _, src := range cfg.Sources {
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

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(cfg.Targets)
		return nil
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
	fmt.Println("mailvault version dev")
}

func main() {
	if err := Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}