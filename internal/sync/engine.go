package sync

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/state"
)

const (
	defaultMaxRetries  = 3
	defaultConcurrency = 5
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
	repair        bool
	refreshFlags  bool
	maxRetries    int
	retryDelay    time.Duration
	concurrency   int
	since         int64
	until         int64
	deliveryLocks sync.Map
	repairCacheMu sync.Mutex
	repairCache   map[string]map[string]string
	runGate       chan struct{}
	progress      func(string, string)
}

// SyncResult holds the result of a sync operation.
type SyncResult struct {
	SourceName      string        `json:"SourceName"`
	TargetName      string        `json:"TargetName"`
	MessagesFetched int           `json:"MessagesFetched"`
	MessagesWritten int           `json:"MessagesWritten"`
	MessagesSkipped int           `json:"MessagesSkipped"`
	Errors          []error       `json:"Errors"`
	Duration        time.Duration `json:"Duration"`
}

// SyncStats holds overall sync statistics.
type SyncStats struct {
	TotalSources       int           `json:"TotalSources"`
	TotalTargets       int           `json:"TotalTargets"`
	TotalPairs         int           `json:"TotalPairs"`
	Results            []SyncResult  `json:"Results"`
	TotalFetched       int           `json:"TotalFetched"`
	TotalWritten       int           `json:"TotalWritten"`
	TotalSkipped       int           `json:"TotalSkipped"`
	TotalErrors        int           `json:"TotalErrors"`
	FinalizationErrors []string      `json:"FinalizationErrors"`
	StartTime          time.Time     `json:"StartTime"`
	EndTime            time.Time     `json:"EndTime"`
	Duration           time.Duration `json:"Duration"`
}

func usedResourceNames(cfg *config.Config) (map[string]bool, map[string]bool) {
	usedSources := make(map[string]bool)
	usedTargets := make(map[string]bool)
	for _, pair := range cfg.Pairs {
		usedSources[pair.Source] = true
		for _, name := range pair.Targets {
			usedTargets[name] = true
		}
	}
	return usedSources, usedTargets
}

func newSyncSources(cfg *config.Config, registry *plugins.Registry, usedSources map[string]bool) (map[string]plugins.Source, error) {
	sources := make(map[string]plugins.Source)
	for i := range cfg.Sources {
		srcCfg := &cfg.Sources[i]
		if !usedSources[srcCfg.Name] {
			continue
		}
		srcConfig, err := config.SourcePluginConfig(srcCfg, cfg.MaxMessageBytes)
		if err != nil {
			return nil, err
		}
		src, err := registry.CreateSource(srcCfg.Type, srcConfig)
		if err != nil {
			return nil, fmt.Errorf("create source %q: %w", srcCfg.Name, err)
		}
		sources[srcCfg.Name] = src
	}
	return sources, nil
}

func newSyncTargets(cfg *config.Config, registry *plugins.Registry, usedTargets map[string]bool) (map[string]plugins.Target, error) {
	targets := make(map[string]plugins.Target)
	for _, tgtCfg := range cfg.Targets {
		if !usedTargets[tgtCfg.Name] {
			continue
		}
		tgtConfig := config.TargetPluginConfig(tgtCfg, cfg.MaxMessageBytes)
		tgt, err := registry.CreateTarget(tgtCfg.Type, tgtConfig)
		if err != nil {
			return nil, fmt.Errorf("create target %q: %w", tgtCfg.Name, err)
		}
		targets[tgtCfg.Name] = tgt
	}
	return targets, nil
}

func validateSyncEngineOptions(engine *SyncEngine) error {
	if engine.concurrency <= 0 || engine.maxRetries < 0 || engine.retryDelay < 0 {
		return errors.New("concurrency must be positive and retry settings must be nonnegative")
	}
	return nil
}

func validateSyncPairs(cfg *config.Config, sources map[string]plugins.Source, targets map[string]plugins.Target) error {
	for _, pair := range cfg.Pairs {
		if sources[pair.Source] == nil {
			return fmt.Errorf("source %q not found for pair", pair.Source)
		}
		for _, name := range pair.Targets {
			if targets[name] == nil {
				return fmt.Errorf("target %q not found for pair", name)
			}
		}
	}
	return nil
}

// NewSyncEngine creates a new sync engine.
func NewSyncEngine(cfg *config.Config, stateStore *state.Store, registry *plugins.Registry, opts ...SyncOption) (*SyncEngine, error) {
	usedSources, usedTargets := usedResourceNames(cfg)
	sources, err := newSyncSources(cfg, registry, usedSources)
	if err != nil {
		return nil, err
	}
	targets, err := newSyncTargets(cfg, registry, usedTargets)
	if err != nil {
		return nil, err
	}
	engine := &SyncEngine{
		config:      cfg,
		stateStore:  stateStore,
		registry:    registry,
		sources:     sources,
		targets:     targets,
		maxRetries:  defaultMaxRetries,
		retryDelay:  time.Second,
		concurrency: defaultConcurrency,
		runGate:     make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(engine)
	}
	if err := validateSyncEngineOptions(engine); err != nil {
		return nil, err
	}
	if err := validateSyncPairs(cfg, sources, targets); err != nil {
		return nil, err
	}
	return engine, nil
}

// SyncOption configures the sync engine.
type SyncOption func(*SyncEngine)

// WithProgress receives concurrent status updates keyed by operation.
func WithProgress(report func(string, string)) SyncOption {
	return func(engine *SyncEngine) { engine.progress = report }
}

// WithForceFetchAll bypasses IMAP download receipts without bypassing content deduplication.
func WithForceFetchAll(force bool) SyncOption {
	return func(engine *SyncEngine) { engine.forceFetchAll = force }
}

// WithRepair rechecks stored manifest entries and writes missing or damaged copies.
func WithRepair(repair bool) SyncOption { return func(engine *SyncEngine) { engine.repair = repair } }

// WithDryRun enables dry-run mode.
func WithDryRun(dryRun bool) SyncOption {
	return func(engine *SyncEngine) {
		engine.dryRun = dryRun
	}
}

// WithMaxRetries sets the maximum number of retries.
func WithMaxRetries(maxRetries int) SyncOption {
	return func(engine *SyncEngine) {
		engine.maxRetries = maxRetries
	}
}

// WithRetryDelay sets the retry delay.
func WithRetryDelay(delay time.Duration) SyncOption {
	return func(engine *SyncEngine) {
		engine.retryDelay = delay
	}
}

// WithConcurrency sets the concurrency limit.
func WithConcurrency(concurrency int) SyncOption {
	return func(engine *SyncEngine) {
		engine.concurrency = concurrency
	}
}

// WithSince sets the since timestamp for filtering messages.
func WithSince(since int64) SyncOption {
	return func(engine *SyncEngine) {
		engine.since = since
	}
}

// WithUntil sets the until timestamp for filtering messages.
func WithUntil(until int64) SyncOption {
	return func(engine *SyncEngine) {
		engine.until = until
	}
}

func newRunStats(engine *SyncEngine) *SyncStats {
	return &SyncStats{
		TotalSources: len(engine.sources),
		TotalTargets: len(engine.targets),
		TotalPairs:   len(engine.config.Pairs),
		StartTime:    time.Now(),
		Results:      make([]SyncResult, 0),
	}
}

// Run executes the full sync process. Setup failures are returned as errors;
// delivery and finalization failures are aggregated in SyncStats.TotalErrors.
func (engine *SyncEngine) Run(ctx context.Context) (_ *SyncStats, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case engine.runGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-engine.runGate }()
	stats := newRunStats(engine)
	var release func() error
	defer func() {
		engine.reportProgress("setup", "closing connections and finalizing state")
		cleanupErr := engine.closeResources(stats)
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
	if !engine.dryRun {
		engine.reportProgress("setup", "waiting for sync lock")
		release, err = engine.stateStore.LockSync(ctx)
		if err != nil {
			return nil, fmt.Errorf("acquire sync lock: %w", err)
		}
	}
	engine.resetDeliveryState()
	if err := engine.initializeRunTargets(ctx); err != nil {
		return nil, err
	}
	if err := engine.upsertRunSources(ctx); err != nil {
		return nil, err
	}
	engine.reportProgress("setup", "processing source-target pairs")
	if err := engine.dispatchPairs(ctx, stats); err != nil {
		return nil, err
	}
	return stats, ctx.Err()
}

func (engine *SyncEngine) resetDeliveryState() {
	engine.deliveryLocks = sync.Map{}
	engine.repairCacheMu.Lock()
	engine.repairCache = nil
	engine.repairCacheMu.Unlock()
}

func (engine *SyncEngine) initializeRunTargets(ctx context.Context) error {
	for _, target := range engine.targets {
		if engine.dryRun {
			continue
		}
		engine.reportProgress("setup", "initializing target "+target.Name())
		if err := target.Initialize(ctx); err != nil {
			return fmt.Errorf("initialize target %q: %w", target.Name(), err)
		}
	}
	return nil
}

func (engine *SyncEngine) upsertRunSources(ctx context.Context) error {
	for _, src := range engine.sources {
		if engine.dryRun {
			continue
		}
		if err := engine.stateStore.UpsertSource(ctx, src.Name(), src.Type(), "", ""); err != nil {
			return fmt.Errorf("upsert source %q: %w", src.Name(), err)
		}
	}
	return nil
}

func (engine *SyncEngine) dispatchPairs(ctx context.Context, stats *SyncStats) error {
	if engine.concurrency <= 0 {
		engine.concurrency = 1
	}
	semaphore := make(chan struct{}, engine.concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
dispatch:
	for _, pair := range engine.config.Pairs {
		src := engine.sources[pair.Source]
		if src == nil {
			return fmt.Errorf("source %q not found for pair", pair.Source)
		}
		for _, targetName := range pair.Targets {
			tgt := engine.targets[targetName]
			if tgt == nil {
				return fmt.Errorf("target %q not found for pair", targetName)
			}
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				break dispatch
			}
			wg.Add(1)
			go func(src plugins.Source, tgt plugins.Target) {
				defer wg.Done()
				defer func() { <-semaphore }()
				paired := engine.syncPair(ctx, src, tgt)
				engine.recordPairResult(&mu, stats, &paired)
			}(src, tgt)
		}
	}
	wg.Wait()
	return nil
}

func (engine *SyncEngine) recordPairResult(mu *sync.Mutex, stats *SyncStats, result *SyncResult) {
	mu.Lock()
	stats.Results = append(stats.Results, *result)
	stats.TotalFetched += result.MessagesFetched
	stats.TotalWritten += result.MessagesWritten
	stats.TotalSkipped += result.MessagesSkipped
	stats.TotalErrors += len(result.Errors)
	mu.Unlock()
}

func (engine *SyncEngine) reportProgress(key, status string) {
	if engine.progress != nil {
		engine.progress(key, status)
	}
}

// syncPair syncs a single source to a single target.
func (engine *SyncEngine) syncPair(ctx context.Context, src plugins.Source, tgt plugins.Target) SyncResult {
	startTime := time.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := SyncResult{
		SourceName: src.Name(),
		TargetName: tgt.Name(),
	}
	folders := engine.getFoldersForSource(src.Name())
	if len(folders) == 0 {
		result.Errors = append(result.Errors, errors.New("no folders configured"))
		result.Duration = time.Since(startTime)
		return result
	}
	key := src.Name() + " -> " + tgt.Name()
	engine.reportProgress(key, "connecting / waiting for source messages")
	report := engine.pairReporter(key, &result)
	defer report("finished")
	ctx = engine.buildPairContext(ctx, src, tgt, key)
	if engine.processPairMessages(ctx, src, tgt, folders, &result, report) {
		result.Duration = time.Since(startTime)
		return result
	}
	if engine.repair && !engine.dryRun {
		result.Errors = append(result.Errors, engine.validateRepairedBackups(ctx, src, tgt)...)
	}
	result.Duration = time.Since(startTime)
	return result
}

func (engine *SyncEngine) pairReporter(key string, result *SyncResult) func(string) {
	writtenLabel := "written"
	if engine.dryRun {
		writtenLabel = "would write"
	}
	return func(stage string) {
		engine.reportProgress(key, fmt.Sprintf("%s: %d fetched, %d %s, %d skipped, %d errors",
			stage, result.MessagesFetched, result.MessagesWritten, writtenLabel, result.MessagesSkipped, len(result.Errors)))
	}
}

func (engine *SyncEngine) buildPairContext(ctx context.Context, src plugins.Source, tgt plugins.Target, key string) context.Context {
	ctx = plugins.WithFetchProgress(ctx, func(progress plugins.FetchProgress) {
		total := "?"
		if progress.Total >= 0 {
			total = strconv.Itoa(progress.Total)
		}
		engine.reportProgress(key+" fetch", fmt.Sprintf("folder %q: %d / %s fetched (%s)",
			progress.Folder, progress.Fetched, total, progress.Stage))
	})
	if !engine.dryRun {
		ctx = plugins.WithFolderStateObserver(ctx, func(observed plugins.FolderStateObservation) error {
			return engine.stateStore.UpsertFolderState(ctx, src.Name(), observed.Folder, &state.FolderState{
				UIDValidity: observed.UIDValidity, UIDNext: observed.UIDNext, ModSeq: observed.ModSeq,
			})
		})
	}
	if !engine.forceFetchAll && !engine.repair {
		ctx = plugins.WithKnownUIDs(ctx, func(scope, folder string) (map[string]bool, error) {
			return engine.stateStore.KnownUIDs(ctx, src.Name(), plugins.TargetStateKey(tgt), targetReceiptScope(tgt, scope), folder)
		})
	}
	if engine.refreshFlags {
		ctx = plugins.WithMetadataObserver(ctx, func(msg *plugins.Message) error {
			return engine.refreshMetadata(ctx, src, tgt, msg)
		})
	}
	return ctx
}

func (engine *SyncEngine) processPairMessages(ctx context.Context, src plugins.Source, tgt plugins.Target, folders []string, result *SyncResult, report func(string)) bool {
	msgChan, errChan := src.FetchMessages(ctx, folders, engine.since, engine.until)
	for msgChan != nil || errChan != nil {
		var done bool
		msgChan, errChan, done = engine.handlePairMessage(ctx, src, tgt, msgChan, errChan, result, report)
		if done {
			return true
		}
	}
	return false
}

func (engine *SyncEngine) handlePairMessage(ctx context.Context, src plugins.Source, tgt plugins.Target, msgChan <-chan *plugins.Message, errChan <-chan error, result *SyncResult, report func(string)) (<-chan *plugins.Message, <-chan error, bool) {
	select {
	case <-ctx.Done():
		result.Errors = append(result.Errors, ctx.Err())
		return msgChan, errChan, true
	case err, ok := <-errChan:
		if !ok {
			return msgChan, nil, false
		}
		if err != nil {
			result.Errors = append(result.Errors, err)
		}
		return msgChan, errChan, false
	case msg, ok := <-msgChan:
		if !ok {
			return nil, errChan, false
		}
		engine.handleFetchedMessage(ctx, src, tgt, msg, result, report)
		return msgChan, errChan, false
	}
}

func (engine *SyncEngine) handleFetchedMessage(ctx context.Context, src plugins.Source, tgt plugins.Target, msg *plugins.Message, result *SyncResult, report func(string)) {
	result.MessagesFetched++
	report("processing message")
	errorsBefore := len(result.Errors)
	engine.syncMessage(ctx, src, tgt, msg, result)
	if engine.refreshFlags && engine.forceFetchAll && !engine.dryRun && msg.UIDScope != "" && len(result.Errors) == errorsBefore {
		if err := engine.refreshMetadata(ctx, src, tgt, msg); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("refresh metadata: %w", err))
		}
	}
	if !engine.dryRun && msg.UIDScope != "" && len(result.Errors) == errorsBefore {
		if err := engine.stateStore.MarkUIDDelivered(ctx, src.Name(), plugins.TargetStateKey(tgt), targetReceiptScope(tgt, msg.UIDScope), msg.Folder, msg.UID); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("persist incremental receipt: %w", err))
		}
	}
	report("waiting for next message")
}

type targetDelivery struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (engine *SyncEngine) lockDelivery(tgt plugins.Target) *targetDelivery {
	value, _ := engine.deliveryLocks.LoadOrStore(tgt.Name(), &targetDelivery{seen: make(map[string]bool)})
	delivery, ok := value.(*targetDelivery)
	if !ok {
		return nil
	}
	return delivery
}

func checkDeliverableMessage(ctx context.Context, msg *plugins.Message, maxBytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return plugins.CheckMessageSize(int64(len(msg.Raw)), maxBytes)
}

func (engine *SyncEngine) resumeSeenDelivery(ctx context.Context, src plugins.Source, tgt plugins.Target, msg *plugins.Message, delivery *targetDelivery, identity, contentHash string, result *SyncResult) bool {
	if !delivery.seen[identity] || engine.repair {
		return false
	}
	if !engine.dryRun {
		if err := engine.recordBackupAlias(ctx, src, tgt, msg, contentHash); err != nil {
			result.Errors = append(result.Errors, err)
			return true
		}
	}
	result.MessagesSkipped++
	return true
}

func (engine *SyncEngine) resumeExistingDelivery(ctx context.Context, src plugins.Source, tgt plugins.Target, msg *plugins.Message, delivery *targetDelivery, stateTarget, identity, contentHash string, result *SyncResult) bool {
	exists, err := engine.stateStore.DeliveryExists(ctx, stateTarget, identity)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("delivery check: %w", err))
		return true
	}
	if !exists || engine.repair {
		return false
	}
	if !engine.dryRun {
		if err := engine.recordBackupAlias(ctx, src, tgt, msg, contentHash); err != nil {
			result.Errors = append(result.Errors, err)
			return true
		}
	}
	delivery.seen[identity] = true
	result.MessagesSkipped++
	return true
}

func (engine *SyncEngine) resumeManifestDelivery(ctx context.Context, src plugins.Source, tgt plugins.Target, msg *plugins.Message, delivery *targetDelivery, stateTarget, identity, contentHash string, result *SyncResult) (plugins.BackupMessage, bool, bool) {
	entry, found, err := engine.stateStore.FindBackupMessage(ctx, src.Name(), stateTarget, targetReceiptScope(tgt, msg.UIDScope), msg.Folder, backupMessageUID(msg, contentHash))
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("find backup manifest entry: %w", err))
		return entry, found, true
	}
	// The manifest commits after the target write and before the receipts.
	// Resume a matching delivery from that checkpoint rather than writing it again.
	if engine.repair || !found || entry.Locator == "" || entry.RawSHA256 != contentHash {
		return entry, found, false
	}
	if !engine.dryRun {
		if err := engine.recordDelivery(ctx, src.Name(), stateTarget, identity, msg, contentHash); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("persist resumed delivery: %w", err))
			return entry, found, true
		}
	}
	delivery.seen[identity] = true
	result.MessagesSkipped++
	return entry, found, true
}

func (engine *SyncEngine) repairStoredBackup(ctx context.Context, src plugins.Source, tgt plugins.Target, msg *plugins.Message, delivery *targetDelivery, stateTarget, identity, contentHash string, entry *plugins.BackupMessage, found bool, result *SyncResult) (string, bool) {
	if !engine.repair {
		return "", false
	}
	var supersededLocator string
	if found {
		locator, done := engine.checkRepairStoredMessage(ctx, src, tgt, msg, delivery, stateTarget, identity, contentHash, entry, result)
		if !done {
			supersededLocator = locator
		} else {
			return "", true
		}
	}
	return engine.scanRepairTarget(ctx, src, tgt, msg, delivery, stateTarget, identity, contentHash, supersededLocator, result)
}

func (engine *SyncEngine) checkRepairStoredMessage(ctx context.Context, src plugins.Source, tgt plugins.Target, msg *plugins.Message, delivery *targetDelivery, stateTarget, identity, contentHash string, entry *plugins.BackupMessage, result *SyncResult) (string, bool) {
	reader, ok := tgt.(plugins.TargetBackupReader)
	if !ok {
		result.Errors = append(result.Errors, fmt.Errorf("target %q does not support verification", tgt.Name()))
		return "", true
	}
	stored, readErr := reader.ReadBackupMessage(ctx, entry.Locator)
	if readErr != nil && !errors.Is(readErr, plugins.ErrBackupMessageMissing) && !errors.Is(readErr, plugins.ErrBackupMessageCorrupt) {
		result.Errors = append(result.Errors, fmt.Errorf("read stored backup %q: %w", entry.Locator, readErr))
		return "", true
	}
	if readErr != nil {
		if errors.Is(readErr, plugins.ErrBackupMessageCorrupt) {
			return entry.Locator, false
		}
		return "", false
	}
	if dedup.ComputeContentHash(stored.Raw) == entry.RawSHA256 && entry.RawSHA256 == contentHash {
		if !engine.dryRun {
			if err := engine.recordDelivery(ctx, src.Name(), stateTarget, identity, msg, contentHash); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("persist repaired delivery: %w", err))
				return "", true
			}
		}
		delivery.seen[identity] = true
		result.MessagesSkipped++
		return "", true
	}
	return entry.Locator, false
}

func (engine *SyncEngine) scanRepairTarget(ctx context.Context, src plugins.Source, tgt plugins.Target, msg *plugins.Message, delivery *targetDelivery, stateTarget, identity, contentHash, supersededLocator string, result *SyncResult) (string, bool) {
	if locator, ok, scanErr := engine.findBackupByHash(ctx, tgt, msg, contentHash); scanErr != nil {
		result.Errors = append(result.Errors, fmt.Errorf("scan target for existing message: %w", scanErr))
		return "", true
	} else if ok {
		if handled := engine.persistRecoveredBackup(ctx, src, tgt, msg, stateTarget, identity, contentHash, locator, supersededLocator, result); handled {
			return "", true
		}
		delivery.seen[identity] = true
		result.MessagesSkipped++
		return "", true
	}
	return supersededLocator, false
}

func (engine *SyncEngine) persistRecoveredBackup(ctx context.Context, src plugins.Source, tgt plugins.Target, msg *plugins.Message, stateTarget, identity, contentHash, locator, supersededLocator string, result *SyncResult) bool {
	if engine.dryRun {
		return false
	}
	if supersededLocator != "" && locator != supersededLocator {
		if err := engine.stateStore.MarkBackupObjectSuperseded(ctx, stateTarget, supersededLocator, locator); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("mark repaired original: %w", err))
			return true
		}
	}
	if err := engine.stateStore.RecordBackupMessage(ctx, &plugins.BackupMessage{Source: src.Name(), Target: stateTarget, Scope: targetReceiptScope(tgt, msg.UIDScope), Folder: msg.Folder, UID: backupMessageUID(msg, contentHash), Locator: locator, RawSHA256: contentHash, Flags: append([]string(nil), msg.Flags...), InternalDate: msg.InternalDate}); err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("persist recovered backup manifest: %w", err))
		return true
	}
	if err := engine.recordDelivery(ctx, src.Name(), stateTarget, identity, msg, contentHash); err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("persist repaired delivery: %w", err))
		return true
	}
	return false
}

func (engine *SyncEngine) writeRepairTarget(ctx context.Context, tgt plugins.Target, msg *plugins.Message, contentHash, supersededLocator, stateTarget, locator string, result *SyncResult) bool {
	if engine.repair && locator != "" {
		engine.rememberRepairBackup(tgt, msg, contentHash, locator)
	}
	if supersededLocator != "" && locator != "" && locator != supersededLocator {
		if err := engine.stateStore.MarkBackupObjectSuperseded(ctx, stateTarget, supersededLocator, locator); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("mark repaired original: %w", err))
			return true
		}
	}
	return false
}

func (engine *SyncEngine) persistWriteDelivery(ctx context.Context, src plugins.Source, tgt plugins.Target, msg *plugins.Message, stateTarget, identity, contentHash, locator string, result *SyncResult) bool {
	if locator != "" {
		if err := engine.stateStore.RecordBackupMessage(ctx, &plugins.BackupMessage{Source: src.Name(), Target: stateTarget, Scope: targetReceiptScope(tgt, msg.UIDScope), Folder: msg.Folder, UID: backupMessageUID(msg, contentHash), Locator: locator, RawSHA256: contentHash, Flags: append([]string(nil), msg.Flags...), InternalDate: msg.InternalDate}); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("persist backup manifest: %w", err))
			return true
		}
	}
	if err := engine.recordDelivery(ctx, src.Name(), stateTarget, identity, msg, contentHash); err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("persist delivery: %w", err))
		return true
	}
	return false
}

func (engine *SyncEngine) syncMessage(ctx context.Context, src plugins.Source, tgt plugins.Target, msg *plugins.Message, result *SyncResult) {
	delivery := engine.lockDelivery(tgt)
	if delivery == nil {
		return // impossible: the lock above is always installed as *targetDelivery
	}
	delivery.mu.Lock()
	defer delivery.mu.Unlock()
	if err := checkDeliverableMessage(ctx, msg, engine.config.MaxMessageBytes); err != nil {
		result.Errors = append(result.Errors, err)
		return
	}
	// Message-ID is sender-controlled and does not prove content equality.
	// Ignore legacy mid: receipts rather than suppress an unverified variant.
	contentHash := dedup.ComputeContentHash(msg.Raw)
	identity := targetDeliveryIdentity(tgt, msg, dedup.DedupKey{ContentHash: contentHash}.Key())
	if engine.resumeSeenDelivery(ctx, src, tgt, msg, delivery, identity, contentHash, result) {
		return
	}
	stateTarget := plugins.TargetStateKey(tgt)
	if engine.resumeExistingDelivery(ctx, src, tgt, msg, delivery, stateTarget, identity, contentHash, result) {
		return
	}
	entry, found, handled := engine.resumeManifestDelivery(ctx, src, tgt, msg, delivery, stateTarget, identity, contentHash, result)
	if handled {
		return
	}
	supersededLocator, handled := engine.repairStoredBackup(ctx, src, tgt, msg, delivery, stateTarget, identity, contentHash, &entry, found, result)
	if handled {
		return
	}
	var locator string
	if !engine.dryRun {
		var err error
		locator, err = engine.writeWithRetry(ctx, tgt, msg, engine.repair)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("write message: %w", err))
			return
		}
		if engine.writeRepairTarget(ctx, tgt, msg, contentHash, supersededLocator, stateTarget, locator, result) {
			return
		}
	}
	result.MessagesWritten++
	if !engine.dryRun {
		if engine.persistWriteDelivery(ctx, src, tgt, msg, stateTarget, identity, contentHash, locator, result) {
			return
		}
	}
	delivery.seen[identity] = true
}

func (engine *SyncEngine) recordDelivery(ctx context.Context, sourceName, stateTarget, identity string, msg *plugins.Message, contentHash string) error {
	return errors.Join(
		engine.stateStore.MarkDelivered(ctx, stateTarget, identity, sourceName, msg.Folder, msg.UID),
		engine.stateStore.MarkMessageFetched(ctx, sourceName, msg.Folder, msg.UID, msg.MessageID, contentHash, msg.Size, msg.InternalDate),
	)
}

func (engine *SyncEngine) recordBackupAlias(ctx context.Context, src plugins.Source, tgt plugins.Target, msg *plugins.Message, rawHash string) error {
	stateTarget := plugins.TargetStateKey(tgt)
	entries, err := engine.stateStore.BackupMessagesByHash(ctx, stateTarget, rawHash)
	if err != nil {
		return fmt.Errorf("find existing backup locations: %w", err)
	}
	identity := targetDeliveryIdentity(tgt, msg, "hash:"+rawHash)
	var entry plugins.BackupMessage
	found := false
	for i := range entries {
		candidate := &entries[i]
		if targetDeliveryIdentity(tgt, &plugins.Message{Folder: candidate.Folder}, "hash:"+rawHash) == identity {
			entry = *candidate
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	return engine.stateStore.RecordBackupMessage(ctx, &plugins.BackupMessage{Source: src.Name(), Target: plugins.TargetStateKey(tgt), Scope: targetReceiptScope(tgt, msg.UIDScope), Folder: msg.Folder, UID: backupMessageUID(msg, rawHash), Locator: entry.Locator, RawSHA256: rawHash, Flags: append([]string(nil), msg.Flags...), InternalDate: msg.InternalDate})
}

func backupMessageUID(msg *plugins.Message, rawHash string) string {
	if msg.UIDScope != "" && msg.UID != "" {
		return msg.UID
	}
	return "content:" + rawHash
}

func (engine *SyncEngine) findBackupByHash(ctx context.Context, tgt plugins.Target, msg *plugins.Message, hash string) (string, bool, error) {
	reader, ok := tgt.(plugins.TargetBackupReader)
	if !ok {
		return "", false, fmt.Errorf("target %q does not support verification", tgt.Name())
	}
	engine.repairCacheMu.Lock()
	if engine.repairCache == nil {
		engine.repairCache = make(map[string]map[string]string)
	}
	index, loaded := engine.repairCache[tgt.Name()]
	if !loaded {
		index = make(map[string]string)
		err := plugins.WalkBackupMessages(ctx, reader, func(entry plugins.BackupMessage) error {
			stored, err := reader.ReadBackupMessage(ctx, entry.Locator)
			if err != nil {
				if errors.Is(err, plugins.ErrBackupMessageMissing) || errors.Is(err, plugins.ErrBackupMessageCorrupt) {
					return nil
				}
				return fmt.Errorf("read %q: %w", entry.Locator, err)
			}
			storedHash := dedup.ComputeContentHash(stored.Raw)
			index[targetDeliveryIdentity(tgt, stored, "hash:"+storedHash)] = entry.Locator
			return nil
		})
		if err != nil {
			engine.repairCacheMu.Unlock()
			return "", false, err
		}
		engine.repairCache[tgt.Name()] = index
	}
	identity := targetDeliveryIdentity(tgt, msg, "hash:"+hash)
	locator, found := index[identity]
	engine.repairCacheMu.Unlock()
	return locator, found, nil
}

func (engine *SyncEngine) rememberRepairBackup(tgt plugins.Target, msg *plugins.Message, hash, locator string) {
	identity := targetDeliveryIdentity(tgt, msg, "hash:"+hash)
	engine.repairCacheMu.Lock()
	defer engine.repairCacheMu.Unlock()
	// Only update a fully loaded index. Creating an index here would make the
	// next lookup treat this one-entry cache as a complete target scan.
	if index, loaded := engine.repairCache[tgt.Name()]; loaded {
		index[identity] = locator
	}
}

func attemptRepairWrite(ctx context.Context, tgt plugins.Target, msg *plugins.Message) (string, error) {
	if writer, ok := tgt.(plugins.RepairBackupWriter); ok {
		return writer.WriteRepairMessage(ctx, msg)
	}
	return "", tgt.WriteMessage(ctx, msg)
}

func attemptBackupWrite(ctx context.Context, tgt plugins.Target, msg *plugins.Message) (string, error) {
	if err := tgt.WriteMessage(ctx, msg); err != nil {
		return "", err
	}
	if provider, ok := tgt.(plugins.BackupLocatorProvider); ok {
		return provider.LastBackupLocator(), nil
	}
	return "", nil
}

func attemptWrite(ctx context.Context, tgt plugins.Target, msg *plugins.Message, repair bool) (string, error) {
	if repair {
		return attemptRepairWrite(ctx, tgt, msg)
	}
	return attemptBackupWrite(ctx, tgt, msg)
}

// writeWithRetry writes a message with retry logic and returns its stable locator.
func (engine *SyncEngine) writeWithRetry(ctx context.Context, tgt plugins.Target, msg *plugins.Message, repair bool) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= engine.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(engine.retryDelay * time.Duration(attempt)):
			}
		}
		locator, err := attemptWrite(ctx, tgt, msg, repair)
		if err == nil {
			return locator, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("after %d retries: %w", engine.maxRetries, lastErr)
}

// getFoldersForSource returns the folders to sync for a source.
func (engine *SyncEngine) getFoldersForSource(sourceName string) []string {
	for i := range engine.config.Sources {
		srcCfg := &engine.config.Sources[i]
		if srcCfg.Name == sourceName {
			if len(srcCfg.Folders) > 0 {
				return srcCfg.Folders
			}
			return []string{"INBOX"}
		}
	}
	return []string{"INBOX"}
}
