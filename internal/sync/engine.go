package sync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/state"
)

// SyncEngine coordinates the sync process between sources and targets.
type SyncEngine struct {
	config       *config.Config
	stateStore   *state.Store
	registry     *plugins.Registry
	sources      map[string]plugins.Source
	targets      map[string]plugins.Target
	dryRun       bool
	maxRetries   int
	retryDelay   time.Duration
	concurrency  int
	since        int64
	until        int64
}

// SyncResult holds the result of a sync operation.
type SyncResult struct {
	SourceName   string
	TargetName   string
	MessagesFetched int
	MessagesWritten int
	MessagesSkipped int
	Errors       []error
	Duration     time.Duration
}

// SyncStats holds overall sync statistics.
type SyncStats struct {
	TotalSources      int
	TotalTargets      int
	TotalPairs        int
	Results           []SyncResult
	TotalFetched      int
	TotalWritten      int
	TotalSkipped      int
	TotalErrors       int
	StartTime         time.Time
	EndTime           time.Time
	Duration          time.Duration
}

// NewSyncEngine creates a new sync engine.
func NewSyncEngine(cfg *config.Config, stateStore *state.Store, registry *plugins.Registry, opts ...SyncOption) (*SyncEngine, error) {
	// Initialize sources
	sources := make(map[string]plugins.Source)
	for _, srcCfg := range cfg.Sources {
		srcConfig := srcConfigToMap(srcCfg)
		srcConfig["name"] = srcCfg.Name
		src, err := registry.CreateSource(srcCfg.Type, srcConfig)
		if err != nil {
			return nil, fmt.Errorf("create source %q: %w", srcCfg.Name, err)
		}
		sources[srcCfg.Name] = src
	}

	// Initialize targets
	targets := make(map[string]plugins.Target)
	for _, tgtCfg := range cfg.Targets {
		tgtConfig := tgtCfg.Config
		if tgtConfig == nil {
			tgtConfig = make(map[string]interface{})
		}
		tgtConfig["name"] = tgtCfg.Name
		tgt, err := registry.CreateTarget(tgtCfg.Type, tgtConfig)
		if err != nil {
			return nil, fmt.Errorf("create target %q: %w", tgtCfg.Name, err)
		}
		targets[tgtCfg.Name] = tgt
	}

	engine := &SyncEngine{
		config:      cfg,
		stateStore:  stateStore,
		registry:    registry,
		sources:     sources,
		targets:     targets,
		maxRetries:  3,
		retryDelay:  time.Second,
		concurrency: 5,
	}

	for _, opt := range opts {
		opt(engine)
	}

	return engine, nil
}

// SyncOption configures the sync engine.
type SyncOption func(*SyncEngine)

// WithDryRun enables dry-run mode.
func WithDryRun(dryRun bool) SyncOption {
	return func(e *SyncEngine) {
		e.dryRun = dryRun
	}
}

// WithMaxRetries sets the maximum number of retries.
func WithMaxRetries(maxRetries int) SyncOption {
	return func(e *SyncEngine) {
		e.maxRetries = maxRetries
	}
}

// WithRetryDelay sets the retry delay.
func WithRetryDelay(delay time.Duration) SyncOption {
	return func(e *SyncEngine) {
		e.retryDelay = delay
	}
}

// WithConcurrency sets the concurrency limit.
func WithConcurrency(concurrency int) SyncOption {
	return func(e *SyncEngine) {
		e.concurrency = concurrency
	}
}

// WithSince sets the since timestamp for filtering messages.
func WithSince(since int64) SyncOption {
	return func(e *SyncEngine) {
		e.since = since
	}
}

// WithUntil sets the until timestamp for filtering messages.
func WithUntil(until int64) SyncOption {
	return func(e *SyncEngine) {
		e.until = until
	}
}

// srcConfigToMap converts SourceConfig to map for plugin factory.
func srcConfigToMap(cfg config.SourceConfig) map[string]interface{} {
	return map[string]interface{}{
		"host":               cfg.Host,
		"port":               cfg.Port,
		"username":           cfg.Username,
		"password_ref":       cfg.PasswordRef,
		"use_tls":            cfg.UseTLS,
		"start_tls":          cfg.StartTLS,
		"folders":            cfg.Folders,
		"connection_timeout": cfg.ConnectionTimeout,
		"read_timeout":       cfg.ReadTimeout,
		"max_connections":    cfg.MaxConnections,
	}
}

// Run executes the full sync process.
func (e *SyncEngine) Run(ctx context.Context) (*SyncStats, error) {
	stats := &SyncStats{
		TotalSources: len(e.sources),
		TotalTargets: len(e.targets),
		TotalPairs:   len(e.config.Pairs),
		StartTime:    time.Now(),
		Results:      make([]SyncResult, 0),
	}

	// Initialize all targets
	for _, target := range e.targets {
		if err := target.Initialize(ctx); err != nil {
			return nil, fmt.Errorf("initialize target %q: %w", target.Name(), err)
		}
	}

	// Upsert sources in state store
	for _, src := range e.sources {
		if err := e.stateStore.UpsertSource(ctx, src.Name(), src.Type(), "", ""); err != nil {
			return nil, fmt.Errorf("upsert source %q: %w", src.Name(), err)
		}
	}

	// Create dedup tracker
	dedupTracker := dedup.NewDedupTracker(&stateAdapter{store: e.stateStore}, "")

	// Process each pair
	semaphore := make(chan struct{}, e.concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, pair := range e.config.Pairs {
		src := e.sources[pair.Source]
		if src == nil {
			return nil, fmt.Errorf("source %q not found for pair", pair.Source)
		}

		for _, targetName := range pair.Targets {
			tgt := e.targets[targetName]
			if tgt == nil {
				return nil, fmt.Errorf("target %q not found for pair", targetName)
			}

			wg.Add(1)
			semaphore <- struct{}{}

			go func(src plugins.Source, tgt plugins.Target, pair config.PairConfig) {
				defer wg.Done()
				defer func() { <-semaphore }()

				result := e.syncPair(ctx, src, tgt, dedupTracker)

				mu.Lock()
				stats.Results = append(stats.Results, result)
				stats.TotalFetched += result.MessagesFetched
				stats.TotalWritten += result.MessagesWritten
				stats.TotalSkipped += result.MessagesSkipped
				stats.TotalErrors += len(result.Errors)
				mu.Unlock()
			}(src, tgt, pair)
		}
	}

	wg.Wait()

	// Close all targets
	for _, target := range e.targets {
		if err := target.Close(); err != nil {
			// Log error but don't fail
			fmt.Printf("Warning: closing target %q: %v\n", target.Name(), err)
		}
	}

	// Close all sources
	for _, src := range e.sources {
		if err := src.Close(); err != nil {
			fmt.Printf("Warning: closing source %q: %v\n", src.Name(), err)
		}
	}

	stats.EndTime = time.Now()
	stats.Duration = stats.EndTime.Sub(stats.StartTime)

	return stats, nil
}

// syncPair syncs a single source to a single target.
func (e *SyncEngine) syncPair(ctx context.Context, src plugins.Source, tgt plugins.Target, dedupTracker *dedup.DedupTracker) SyncResult {
	startTime := time.Now()
	result := SyncResult{
		SourceName: src.Name(),
		TargetName: tgt.Name(),
	}

	// Determine folders to sync
	folders := e.getFoldersForSource(src.Name())
	if len(folders) == 0 {
		result.Errors = append(result.Errors, errors.New("no folders configured"))
		return result
	}

	// Fetch messages from source
	msgChan, errChan := src.FetchMessages(ctx, folders, e.since, e.until)

	// Process messages
	for {
		select {
		case <-ctx.Done():
			result.Errors = append(result.Errors, ctx.Err())
			result.Duration = time.Since(startTime)
			return result
		case err := <-errChan:
			if err != nil {
				result.Errors = append(result.Errors, err)
			}
		case msg, ok := <-msgChan:
			if !ok {
				// Channel closed, done fetching
				result.Duration = time.Since(startTime)
				return result
			}

			result.MessagesFetched++

			// Check deduplication
			skip, err := dedupTracker.ShouldSkip(ctx, msg, msg.Folder)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("dedup check: %w", err))
				continue
			}
			if skip {
				result.MessagesSkipped++
				continue
			}

			// Write to target (with retry)
			if !e.dryRun {
				if err := e.writeWithRetry(ctx, tgt, msg); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("write message: %w", err))
					continue
				}
			}

			result.MessagesWritten++

			// Mark in dedup tracker
			dedupTracker.MarkSeen(msg)

			// Persist to state store
			if !e.dryRun {
				if err := e.stateStore.MarkMessageFetched(ctx, src.Name(), msg.Folder, msg.UID, msg.MessageID, msg.ContentHash, msg.Size, msg.InternalDate); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("persist state: %w", err))
				}
			}
		}
	}
}

// writeWithRetry writes a message with retry logic.
func (e *SyncEngine) writeWithRetry(ctx context.Context, tgt plugins.Target, msg *plugins.Message) error {
	var lastErr error
	for attempt := 0; attempt <= e.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(e.retryDelay * time.Duration(attempt)):
			}
		}

		err := tgt.WriteMessage(ctx, msg)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("after %d retries: %w", e.maxRetries, lastErr)
}

// getFoldersForSource returns the folders to sync for a source.
func (e *SyncEngine) getFoldersForSource(sourceName string) []string {
	for _, srcCfg := range e.config.Sources {
		if srcCfg.Name == sourceName {
			if len(srcCfg.Folders) > 0 {
				return srcCfg.Folders
			}
			return []string{"INBOX"}
		}
	}
	return []string{"INBOX"}
}

// stateAdapter adapts state.Store to dedup.StateStore interface.
type stateAdapter struct {
	store *state.Store
}

func (a *stateAdapter) MessageExists(ctx context.Context, sourceName, folder, uid string) (bool, error) {
	return a.store.MessageExists(ctx, sourceName, folder, uid)
}

func (a *stateAdapter) FindByMessageID(ctx context.Context, messageID string) ([]dedup.MessageRecord, error) {
	records, err := a.store.FindByMessageID(ctx, messageID)
	if err != nil {
		return nil, err
	}
	result := make([]dedup.MessageRecord, len(records))
	for i, r := range records {
		result[i] = dedup.MessageRecord{
			SourceName: r.SourceName,
			Folder:     r.Folder,
			UID:        r.UID,
		}
	}
	return result, nil
}

func (a *stateAdapter) FindByContentHash(ctx context.Context, contentHash string) ([]dedup.MessageRecord, error) {
	records, err := a.store.FindByContentHash(ctx, contentHash)
	if err != nil {
		return nil, err
	}
	result := make([]dedup.MessageRecord, len(records))
	for i, r := range records {
		result[i] = dedup.MessageRecord{
			SourceName: r.SourceName,
			Folder:     r.Folder,
			UID:        r.UID,
		}
	}
	return result, nil
}