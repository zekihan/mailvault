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
	config        *config.Config
	stateStore    *state.Store
	registry      *plugins.Registry
	sources       map[string]plugins.Source
	targets       map[string]plugins.Target
	dryRun        bool
	forceFetchAll bool
	maxRetries    int
	retryDelay    time.Duration
	concurrency   int
	since         int64
	until         int64
	deliveryLocks sync.Map
	runGate       chan struct{}
	progress      func(string, string)
}

// SyncResult holds the result of a sync operation.
type SyncResult struct {
	SourceName      string
	TargetName      string
	MessagesFetched int
	MessagesWritten int
	MessagesSkipped int
	Errors          []error
	Duration        time.Duration
}

// SyncStats holds overall sync statistics.
type SyncStats struct {
	TotalSources       int
	TotalTargets       int
	TotalPairs         int
	Results            []SyncResult
	TotalFetched       int
	TotalWritten       int
	TotalSkipped       int
	TotalErrors        int
	FinalizationErrors []string
	StartTime          time.Time
	EndTime            time.Time
	Duration           time.Duration
}

// NewSyncEngine creates a new sync engine.
func NewSyncEngine(cfg *config.Config, stateStore *state.Store, registry *plugins.Registry, opts ...SyncOption) (*SyncEngine, error) {
	usedSources, usedTargets := make(map[string]bool), make(map[string]bool)
	for _, pair := range cfg.Pairs {
		usedSources[pair.Source] = true
		for _, name := range pair.Targets {
			usedTargets[name] = true
		}
	}
	// Initialize sources
	sources := make(map[string]plugins.Source)
	for _, srcCfg := range cfg.Sources {
		if !usedSources[srcCfg.Name] {
			continue
		}
		if srcCfg.OAuth2 != nil && srcCfg.PasswordRef == "" {
			return nil, fmt.Errorf("source %q: OAuth2 authentication is not implemented; password_ref is required", srcCfg.Name)
		}
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
		if !usedTargets[tgtCfg.Name] {
			continue
		}
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
		runGate:     make(chan struct{}, 1),
	}

	for _, opt := range opts {
		opt(engine)
	}
	if engine.concurrency <= 0 || engine.maxRetries < 0 || engine.retryDelay < 0 {
		return nil, errors.New("concurrency must be positive and retry settings must be nonnegative")
	}
	for _, pair := range cfg.Pairs {
		if sources[pair.Source] == nil {
			return nil, fmt.Errorf("source %q not found for pair", pair.Source)
		}
		for _, name := range pair.Targets {
			if targets[name] == nil {
				return nil, fmt.Errorf("target %q not found for pair", name)
			}
		}
	}
	if engine.since != 0 || engine.until != 0 {
		for _, src := range sources {
			if src.Type() == "pop3" {
				return nil, errors.New("POP3 date filters are unsupported; omit --since and --until")
			}
		}
	}

	return engine, nil
}

// SyncOption configures the sync engine.
type SyncOption func(*SyncEngine)

// WithProgress receives concurrent status updates keyed by operation.
func WithProgress(report func(string, string)) SyncOption {
	return func(e *SyncEngine) { e.progress = report }
}

func (e *SyncEngine) reportProgress(key, status string) {
	if e.progress != nil {
		e.progress(key, status)
	}
}

// WithForceFetchAll bypasses IMAP download receipts without bypassing content deduplication.
func WithForceFetchAll(force bool) SyncOption {
	return func(e *SyncEngine) { e.forceFetchAll = force }
}

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

// Run executes the full sync process. Setup failures are returned as errors;
// delivery and finalization failures are aggregated in SyncStats.TotalErrors.
func (e *SyncEngine) Run(ctx context.Context) (_ *SyncStats, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case e.runGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-e.runGate }()
	stats := &SyncStats{
		TotalSources: len(e.sources),
		TotalTargets: len(e.targets),
		TotalPairs:   len(e.config.Pairs),
		StartTime:    time.Now(),
		Results:      make([]SyncResult, 0),
	}
	var release func() error
	defer func() {
		e.reportProgress("setup", "closing connections and finalizing state")
		cleanupErr := e.closeResources(stats)
		if release != nil {
			if releaseErr := release(); releaseErr != nil {
				wrapped := fmt.Errorf("release sync lock: %w", releaseErr)
				stats.FinalizationErrors = append(stats.FinalizationErrors, wrapped.Error())
				stats.TotalErrors++
				cleanupErr = errors.Join(cleanupErr, wrapped)
			}
		}
		if err != nil {
			err = errors.Join(err, cleanupErr)
		}
		stats.EndTime = time.Now()
		stats.Duration = stats.EndTime.Sub(stats.StartTime)
	}()
	if !e.dryRun {
		e.reportProgress("setup", "waiting for sync lock")
		release, err = e.stateStore.LockSync(ctx)
		if err != nil {
			return nil, fmt.Errorf("acquire sync lock: %w", err)
		}
	}
	e.deliveryLocks = sync.Map{}

	// Initialize all targets
	for _, target := range e.targets {
		if e.dryRun {
			continue
		}
		e.reportProgress("setup", fmt.Sprintf("initializing target %s", target.Name()))
		if err := target.Initialize(ctx); err != nil {
			return nil, fmt.Errorf("initialize target %q: %w", target.Name(), err)
		}
	}

	// Upsert sources in state store
	for _, src := range e.sources {
		if e.dryRun {
			continue
		}
		if err := e.stateStore.UpsertSource(ctx, src.Name(), src.Type(), "", ""); err != nil {
			return nil, fmt.Errorf("upsert source %q: %w", src.Name(), err)
		}
	}

	e.reportProgress("setup", "processing source-target pairs")

	// Process each pair
	if e.concurrency <= 0 {
		e.concurrency = 1
	}
	semaphore := make(chan struct{}, e.concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

dispatch:
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

			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				break dispatch
			}
			wg.Add(1)

			go func(src plugins.Source, tgt plugins.Target, pair config.PairConfig) {
				defer wg.Done()
				defer func() { <-semaphore }()

				result := e.syncPair(ctx, src, tgt)

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

	return stats, ctx.Err()
}

// syncPair syncs a single source to a single target.
func (e *SyncEngine) syncPair(ctx context.Context, src plugins.Source, tgt plugins.Target) (result SyncResult) {
	startTime := time.Now()
	defer func() { result.Duration = time.Since(startTime) }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	result = SyncResult{
		SourceName: src.Name(),
		TargetName: tgt.Name(),
	}

	// Determine folders to sync
	folders := e.getFoldersForSource(src.Name())
	if len(folders) == 0 {
		result.Errors = append(result.Errors, errors.New("no folders configured"))
		return result
	}

	key := src.Name() + " -> " + tgt.Name()
	e.reportProgress(key, "connecting / waiting for source messages")
	writtenLabel := "written"
	if e.dryRun {
		writtenLabel = "would write"
	}
	report := func(stage string) {
		e.reportProgress(key, fmt.Sprintf("%s: %d fetched, %d %s, %d skipped, %d errors",
			stage, result.MessagesFetched, result.MessagesWritten, writtenLabel, result.MessagesSkipped, len(result.Errors)))
	}
	defer func() { report("finished") }()

	ctx = plugins.WithFetchProgress(ctx, func(progress plugins.FetchProgress) {
		total := "?"
		if progress.Total >= 0 {
			total = fmt.Sprint(progress.Total)
		}
		e.reportProgress(key+" fetch", fmt.Sprintf("folder %q: %d / %s fetched (%s)",
			progress.Folder, progress.Fetched, total, progress.Stage))
	})

	if !e.forceFetchAll {
		ctx = plugins.WithKnownUIDs(ctx, func(scope, folder string) (map[string]bool, error) {
			return e.stateStore.KnownUIDs(ctx, src.Name(), tgt.Name(), targetReceiptScope(tgt, scope), folder)
		})
	}

	// Fetch messages from source
	msgChan, errChan := src.FetchMessages(ctx, folders, e.since, e.until)

	// Process messages
	for msgChan != nil || errChan != nil {
		select {
		case <-ctx.Done():
			result.Errors = append(result.Errors, ctx.Err())
			return result
		case err, ok := <-errChan:
			if !ok {
				errChan = nil
			} else if err != nil {
				result.Errors = append(result.Errors, err)
			}
		case msg, ok := <-msgChan:
			if !ok {
				msgChan = nil
				continue
			}

			result.MessagesFetched++
			report("processing message")
			errorsBefore := len(result.Errors)
			e.syncMessage(ctx, src, tgt, msg, &result)
			if !e.dryRun && msg.UIDScope != "" && len(result.Errors) == errorsBefore {
				if err := e.stateStore.MarkUIDDelivered(ctx, src.Name(), tgt.Name(), targetReceiptScope(tgt, msg.UIDScope), msg.Folder, msg.UID); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("persist IMAP receipt: %w", err))
				}
			}
			report("waiting for next message")
		}
	}
	return result
}

type targetDelivery struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (e *SyncEngine) syncMessage(ctx context.Context, src plugins.Source, tgt plugins.Target, msg *plugins.Message, result *SyncResult) {
	value, _ := e.deliveryLocks.LoadOrStore(tgt.Name(), &targetDelivery{seen: make(map[string]bool)})
	delivery := value.(*targetDelivery)
	delivery.mu.Lock()
	defer delivery.mu.Unlock()
	if err := ctx.Err(); err != nil {
		result.Errors = append(result.Errors, err)
		return
	}
	// Message-ID is sender-controlled and does not prove content equality.
	// Ignore legacy mid: receipts rather than suppress an unverified variant.
	contentHash := dedup.ComputeContentHash(msg.Raw)
	identity := targetDeliveryIdentity(tgt, msg, dedup.DedupKey{ContentHash: contentHash}.Key())
	if delivery.seen[identity] {
		result.MessagesSkipped++
		return
	}
	exists, err := e.stateStore.DeliveryExists(ctx, tgt.Name(), identity)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("delivery check: %w", err))
		return
	}
	if exists {
		delivery.seen[identity] = true
		result.MessagesSkipped++
		return
	}
	if !e.dryRun {
		if err := e.writeWithRetry(ctx, tgt, msg); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("write message: %w", err))
			return
		}
	}
	delivery.seen[identity] = true
	result.MessagesWritten++
	if !e.dryRun {
		err := errors.Join(
			e.stateStore.MarkDelivered(ctx, tgt.Name(), identity, src.Name(), msg.Folder, msg.UID),
			e.stateStore.MarkMessageFetched(ctx, src.Name(), msg.Folder, msg.UID, msg.MessageID, contentHash, msg.Size, msg.InternalDate),
		)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("persist delivery: %w", err))
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
