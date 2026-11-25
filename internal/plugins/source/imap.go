package source

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/zekihan/mailvault/internal/auth"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/config"
	"sync"
)

const (
	defaultIMAPPort          = 993
	defaultConnectionTimeout = 30 * time.Second
	defaultReadTimeout       = 60 * time.Second
	defaultMaxConnections    = 5
)

// IMAPSource implements the Source interface for IMAP.
type IMAPSource struct {
	limits          listingLimits
	listing         *listingBudgetConn
	name            string
	host            string
	port            int
	username        string
	password        string
	useTLS          bool
	startTLS        bool
	folders         []string
	connTimeout     time.Duration
	readTimeout     time.Duration
	maxMessageBytes int64
	maxConns        int
	connectRetry    connectBackoff
	tokenSource     func(context.Context) (string, error)
	client          *imapclient.Client
	wire            *deadlineConn
	connected       bool
	mu              sync.Mutex
}

type imapSession struct {
	listing *listingBudgetConn
	client  *imapclient.Client
	wire    *deadlineConn
}

type parsedIMAPConfig struct {
	limits             listingLimits
	maxMessageBytes    int64
	host               string
	port               int
	username           string
	passwordRef        string
	useTLS, startTLS   bool
	folders            []string
	connTimeout        time.Duration
	readTimeout        time.Duration
	maxConns           int
	connectRetryBudget time.Duration
	tokenSource        func(context.Context) (string, error)
}

func NewIMapSource(configMap map[string]any) (plugins.Source, error) {
	parsed, err := parseIMAPConfig(configMap)
	if err != nil {
		return nil, err
	}
	if err := validateIMAPConfig(&parsed); err != nil {
		return nil, err
	}
	return &IMAPSource{
		limits:          parsed.limits,
		name:            "imap:" + parsed.host, // Will be overridden by registry
		host:            parsed.host,
		port:            parsed.port,
		username:        parsed.username,
		password:        parsed.passwordRef, // Already resolved by config loader
		useTLS:          parsed.useTLS,
		startTLS:        parsed.startTLS,
		folders:         parsed.folders,
		connTimeout:     parsed.connTimeout,
		readTimeout:     parsed.readTimeout,
		maxConns:        parsed.maxConns,
		connectRetry:    connectBackoff{budget: parsed.connectRetryBudget},
		maxMessageBytes: parsed.maxMessageBytes,
		tokenSource:     parsed.tokenSource,
	}, nil
}

func parseIMAPConfig(configMap map[string]any) (parsedIMAPConfig, error) {
	var parsed parsedIMAPConfig
	if err := parseIMAPConfigValues(configMap, &parsed); err != nil {
		return parsedIMAPConfig{}, err
	}
	return parsed, nil
}

func parseIMAPConfigValues(configMap map[string]any, parsed *parsedIMAPConfig) error {
	var err error
	parsed.limits, err = parseListingLimits(configMap)
	if err != nil {
		return err
	}
	parsed.maxMessageBytes, err = config.ParseMessageLimit(configMap)
	if err != nil {
		return err
	}
	return parseIMAPConfigStrings(configMap, parsed)
}

func parseIMAPConfigStrings(configMap map[string]any, parsed *parsedIMAPConfig) error {
	var err error
	if parsed.host, err = parseIMAPString(configMap, config.SourceConfigHost); err != nil {
		return err
	}
	if parsed.port, err = parseIMAPInt(configMap, config.SourceConfigPort, defaultIMAPPort); err != nil {
		return err
	}
	if parsed.username, err = parseIMAPString(configMap, config.SourceConfigUsername); err != nil {
		return err
	}
	if parsed.passwordRef, err = parseIMAPString(configMap, config.SourceConfigPasswordRef); err != nil {
		return err
	}
	return parseIMAPConfigFlags(configMap, parsed)
}

func parseIMAPConfigFlags(configMap map[string]any, parsed *parsedIMAPConfig) error {
	var err error
	if parsed.useTLS, err = parseIMAPBool(configMap, config.SourceConfigUseTLS, true); err != nil {
		return err
	}
	if parsed.startTLS, err = parseIMAPBool(configMap, config.SourceConfigStartTLS, false); err != nil {
		return err
	}
	return parseIMAPConfigLimits(configMap, parsed)
}

func parseIMAPConfigLimits(configMap map[string]any, parsed *parsedIMAPConfig) error {
	var err error
	if parsed.folders, err = config.ParseStringSlice(configMap, config.SourceConfigFolders); err != nil {
		return fmt.Errorf("%s: %w", config.SourceConfigFolders, err)
	}
	if parsed.connTimeout, err = parseIMAPDuration(configMap, config.SourceConfigConnectionTimeout, defaultConnectionTimeout); err != nil {
		return err
	}
	if parsed.readTimeout, err = parseIMAPDuration(configMap, config.SourceConfigReadTimeout, defaultReadTimeout); err != nil {
		return err
	}
	if parsed.maxConns, err = parseIMAPInt(configMap, config.SourceConfigMaxConnections, defaultMaxConnections); err != nil {
		return err
	}
	return parseIMAPRetry(configMap, parsed)
}

func parseIMAPRetry(configMap map[string]any, parsed *parsedIMAPConfig) error {
	budget, err := config.ParseDuration(configMap, config.SourceConfigConnectRetryTimeout, DefaultConnectRetryTimeout)
	if err != nil {
		return fmt.Errorf("%s: %w", config.SourceConfigConnectRetryTimeout, err)
	}
	if budget < 0 {
		return fmt.Errorf("%s must be >= 0", config.SourceConfigConnectRetryTimeout)
	}
	parsed.connectRetryBudget = budget
	if v, ok := configMap["oauth2_token_provider"].(func(context.Context) (string, error)); ok {
		parsed.tokenSource = v
	}
	return nil
}

func parseIMAPString(configMap map[string]any, key string) (string, error) {
	value, err := config.ParseString(configMap, key, "")
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	return value, nil
}

func parseIMAPInt(configMap map[string]any, key string, fallback int) (int, error) {
	value, err := config.ParseInt(configMap, key, fallback)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return value, nil
}

func parseIMAPBool(configMap map[string]any, key string, fallback bool) (bool, error) {
	value, err := config.ParseBool(configMap, key, fallback)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return value, nil
}

func parseIMAPDuration(configMap map[string]any, key string, fallback time.Duration) (time.Duration, error) {
	value, err := config.ParseDuration(configMap, key, fallback)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return value, nil
}

func validateIMAPConfig(parsed *parsedIMAPConfig) error {
	if parsed.host == "" {
		return errors.New("host is required")
	}
	if parsed.username == "" {
		return errors.New("username is required")
	}
	if parsed.passwordRef == "" && parsed.tokenSource == nil {
		return errors.New("password_ref is required")
	}
	if parsed.maxConns <= 0 {
		parsed.maxConns = 5
	}
	if parsed.useTLS && parsed.startTLS {
		return errors.New("use_tls and start_tls cannot both be enabled")
	}
	if parsed.tokenSource != nil && !parsed.useTLS && !parsed.startTLS {
		return errors.New("IMAP OAuth2 requires TLS; enable use_tls or start_tls")
	}
	if len(parsed.folders) == 0 {
		parsed.folders = []string{"INBOX"}
	}
	return nil
}

// SetName sets the source name (called by registry).
func (src *IMAPSource) SetName(name string) {
	src.name = name
}

func (src *IMAPSource) Name() string {
	return src.name
}

func (src *IMAPSource) Type() string {
	return "imap"
}

func (src *IMAPSource) Connect(ctx context.Context) error {
	src.mu.Lock()
	defer src.mu.Unlock()
	return src.connectLocked(ctx)
}

// isIMAPRejection reports a tagged NO/BAD response, which reconnecting cannot fix.
func isIMAPRejection(err error) bool {
	var statusErr *imap.Error
	return errors.As(err, &statusErr) && !temporaryIMAPFailure(err) &&
		(statusErr.Type == imap.StatusResponseTypeNo || statusErr.Type == imap.StatusResponseTypeBad)
}

func (src *IMAPSource) ListFolders(ctx context.Context) ([]string, error) {
	src.mu.Lock()
	defer src.mu.Unlock()
	if err := src.connectLocked(ctx); err != nil {
		return nil, err
	}
	finish := src.wire.beginIO(ctx)
	defer finish()

	var folders []string
	err := src.listMailboxesLocked(func(mb *imap.ListData) {
		if !slices.Contains(mb.Attrs, imap.MailboxAttrNoSelect) {
			folders = append(folders, mb.Mailbox)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	return folders, nil
}

func (src *IMAPSource) FetchMessages(ctx context.Context, folders []string, since, until int64) (<-chan *plugins.Message, <-chan error) {
	msgChan := make(chan *plugins.Message)
	errChan := make(chan error, 1)

	go func() {
		defer close(msgChan)
		defer close(errChan)
		src.mu.Lock()
		defer src.mu.Unlock()

		err := retryFetch(ctx, msgChan, func(ctx context.Context, output chan<- *plugins.Message) error {
			if err := src.connectLocked(ctx); err != nil {
				return err
			}
			sessions := []*imapSession{{client: src.client, wire: src.wire, listing: src.listing}}
			for _, folder := range folders {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := src.fetchFolder(ctx, folder, output, since, until, sessions); err != nil {
					if closeErr := src.closeLocked(); closeErr != nil {
						return errors.Join(fmt.Errorf("fetch folder %q: %w", folder, err), fmt.Errorf("close: %w", closeErr))
					}
					return fmt.Errorf("fetch folder %q: %w", folder, err)
				}
			}
			return nil
		})
		if err != nil {
			errChan <- err
		}
	}()

	return msgChan, errChan
}

func filterPendingUIDs(uids []imap.UID, known map[string]bool) []imap.UID {
	pending := make([]imap.UID, 0, len(uids))
	for _, uid := range uids {
		if !known[strconv.FormatUint(uint64(uid), 10)] {
			pending = append(pending, uid)
		}
	}
	return pending
}

func fetchWorkerSelectionErr(workerCtx context.Context, session *imapSession, folder string, err error) error {
	if workerCtx.Err() != nil {
		return err
	}
	return fmt.Errorf("select %q for fetch: %w", folder, joinTransportError(err, session.wire))
}

func fetchWorkerBatchErr(workerCtx context.Context, session *imapSession, err error) error {
	if workerCtx.Err() != nil {
		return err
	}
	return joinTransportError(err, session.wire)
}

func buildFetchedMessage(buf *imapclient.FetchMessageBuffer, folder, scope string, raw []byte) *plugins.Message {
	// Parse headers for Message-ID.
	messageID := extractMessageIDFromEnvelope(buf.Envelope)
	if messageID == "" {
		// Fallback: parse from raw.
		messageID = dedup.ExtractMessageID(raw)
	}
	// Compute content hash.
	contentHash := dedup.ComputeContentHash(raw)
	return &plugins.Message{
		UIDScope:     scope,
		UID:          strconv.FormatUint(uint64(buf.UID), 10),
		Folder:       folder,
		Raw:          raw,
		MessageID:    messageID,
		ContentHash:  contentHash,
		Size:         int64(len(raw)),
		InternalDate: fetchedInternalDate(buf),
		Flags:        fetchedFlagStrings(buf),
	}
}

// Prefer the server's arrival date to the sender's Date header.
func fetchedInternalDate(buf *imapclient.FetchMessageBuffer) string {
	if !buf.InternalDate.IsZero() {
		return buf.InternalDate.Format(time.RFC3339)
	}
	if buf.Envelope != nil && !buf.Envelope.Date.IsZero() {
		return buf.Envelope.Date.Format(time.RFC3339)
	}
	return ""
}

func fetchedFlagStrings(buf *imapclient.FetchMessageBuffer) []string {
	flags := make([]string, 0, len(buf.Flags))
	for _, flag := range buf.Flags {
		flags = append(flags, string(flag))
	}
	return flags
}

func collectFetchMessage(data *imapclient.FetchMessageData, maxBytes int64, abort func()) (*imapclient.FetchMessageBuffer, error) {
	buf := &imapclient.FetchMessageBuffer{SeqNum: data.SeqNum}
	var buffered int64
	reject := func(err error) (*imapclient.FetchMessageBuffer, error) {
		// Close before discarding: only already-buffered bytes remain. Next
		// releases each literal's decoder waiter once, without allocating it.
		abort()
		for item := data.Next(); item != nil; item = data.Next() {
			_ = item // drain buffered data before abandoning the buffer
		}
		return nil, err
	}
	for item := data.Next(); item != nil; item = data.Next() {
		nextBuffered, err := collectFetchItem(buf, item, maxBytes, buffered, reject)
		if err != nil {
			return nil, err
		}
		buffered = nextBuffered
	}
	return buf, nil
}

func collectFetchItem(buf *imapclient.FetchMessageBuffer, item any, maxBytes, buffered int64, reject func(error) (*imapclient.FetchMessageBuffer, error)) (int64, error) {
	switch item := item.(type) {
	case imapclient.FetchItemDataFlags:
		buf.Flags = item.Flags
	case imapclient.FetchItemDataEnvelope:
		buf.Envelope = item.Envelope
	case imapclient.FetchItemDataInternalDate:
		buf.InternalDate = item.Time
	case imapclient.FetchItemDataUID:
		buf.UID = item.UID
	case imapclient.FetchItemDataRFC822Size:
		buf.RFC822Size = item.Size
	case imapclient.FetchItemDataModSeq:
		buf.ModSeq = item.ModSeq
	case imapclient.FetchItemDataBodyStructure:
		buf.BodyStructure = item.BodyStructure
	case imapclient.FetchItemDataBodySection:
		body, next, err := collectFetchLiteral(item.Literal, maxBytes, buffered, "body", reject)
		if err != nil {
			return next, err
		}
		buffered = next
		buf.BodySection = append(buf.BodySection, imapclient.FetchBodySectionBuffer{Section: item.Section, Bytes: body})
	case imapclient.FetchItemDataBinarySection:
		body, next, err := collectFetchLiteral(item.Literal, maxBytes, buffered, "binary", reject)
		if err != nil {
			return next, err
		}
		buffered = next
		buf.BinarySection = append(buf.BinarySection, imapclient.FetchBinarySectionBuffer{Section: item.Section, Bytes: body})
	case imapclient.FetchItemDataBinarySectionSize:
		buf.BinarySectionSize = append(buf.BinarySectionSize, item)
	}
	return buffered, nil
}

func collectFetchLiteral(literal imap.LiteralReader, maxBytes, buffered int64, kind string, reject func(error) (*imapclient.FetchMessageBuffer, error)) ([]byte, int64, error) {
	if literal == nil {
		return nil, buffered, nil
	}
	want := literal.Size()
	if want < 0 || uint64(want) > uint64(^uint(0)>>1) {
		_, err := reject(fmt.Errorf("%w: invalid %s literal size %d", plugins.ErrMessageTooLarge, kind, want))
		return nil, buffered, err
	}
	if err := plugins.CheckMessageSize(buffered+want, maxBytes); err != nil {
		_, err := reject(err)
		return nil, buffered, err
	}
	body := make([]byte, int(want))
	if _, err := io.ReadFull(literal, body); err != nil {
		return nil, buffered, err
	}
	return body, buffered + want, nil
}

func (src *IMAPSource) Close() error {
	src.mu.Lock()
	defer src.mu.Unlock()
	return src.closeLocked()
}

func (src *IMAPSource) fetchFolder(ctx context.Context, folder string, msgChan chan<- *plugins.Message, since, until int64, sessions []*imapSession) error {
	progress := plugins.FetchProgress{Folder: folder, Total: -1, Stage: "searching"}
	plugins.ReportFetchProgress(ctx, progress)

	selectedUIDValidity, scope, uids, err := src.planFolderFetch(ctx, folder, since, until, sessions)
	if err != nil {
		return err
	}
	progress.Total = len(uids)
	progress.Stage = "fetching"
	plugins.ReportFetchProgress(ctx, progress)
	completed := func() {
		progress.Stage = "finished"
		plugins.ReportFetchProgress(ctx, progress)
	}
	if len(uids) == 0 {
		completed()
		return nil
	}
	err = src.fetchFolderBatches(ctx, folder, msgChan, since, until, sessions, uids, selectedUIDValidity, scope, &progress)
	if err != nil {
		return err
	}
	completed()
	return nil
}

func (src *IMAPSource) planFolderFetch(ctx context.Context, folder string, since, until int64, sessions []*imapSession) (uint32, string, []imap.UID, error) {
	selected, err := src.selectFolderMailbox(ctx, folder, sessions[0])
	if err != nil {
		return 0, "", nil, err
	}
	scope, known, err := src.loadFolderKnownUIDs(ctx, folder, selected)
	if err != nil {
		return 0, "", nil, err
	}
	if err := src.refreshMetadata(ctx, sessions[0], scope, folder, known); err != nil {
		return 0, "", nil, fmt.Errorf("refresh metadata: %w", err)
	}
	known = skipStreamedUIDs(ctx, scope, folder, known)
	searchData, err := src.searchFolderUIDs(ctx, since, until, sessions[0], known)
	if err != nil {
		return 0, "", nil, err
	}
	uids, err := boundedSearchUIDs(searchData, src.limits.defaults().items)
	if err != nil {
		return 0, "", nil, fmt.Errorf("search: %w", err)
	}
	return selected.UIDValidity, scope, filterPendingUIDs(uids, known), nil
}

func (src *IMAPSource) selectFolderMailbox(ctx context.Context, folder string, session *imapSession) (*imap.SelectData, error) {
	// Select mailbox.
	finish := session.wire.beginIO(ctx)
	selectCmd := session.client.Select(folder, &imap.SelectOptions{ReadOnly: true})
	selected, err := selectCmd.Wait()
	finish()
	if err != nil {
		return nil, fmt.Errorf("select %q: %w", folder, joinTransportError(err, session.wire))
	}
	observation := plugins.FolderStateObservation{Folder: folder}
	if selected.UIDValidity != 0 {
		observation.UIDValidity = strconv.FormatUint(uint64(selected.UIDValidity), 10)
	}
	if selected.UIDNext != 0 {
		observation.UIDNext = strconv.FormatUint(uint64(selected.UIDNext), 10)
	}
	if selected.HighestModSeq != 0 {
		observation.ModSeq = strconv.FormatUint(selected.HighestModSeq, 10)
	}
	if err := plugins.ObserveFolderState(ctx, observation); err != nil {
		return nil, fmt.Errorf("record folder state: %w", err)
	}
	return selected, nil
}

func (src *IMAPSource) loadFolderKnownUIDs(ctx context.Context, folder string, selected *imap.SelectData) (string, map[string]bool, error) {
	scope := ""
	var known map[string]bool
	if selected.UIDValidity != 0 {
		scope = dedup.ComputeContentHash([]byte(fmt.Sprintf("%s:%d\x00%s\x00%d", src.host, src.port, src.username, selected.UIDValidity)))
		var err error
		known, err = plugins.KnownUIDs(ctx, scope, folder)
		if err != nil {
			return "", nil, fmt.Errorf("load incremental state: %w", err)
		}
	}
	return scope, known, nil
}

func (src *IMAPSource) searchFolderUIDs(ctx context.Context, since, until int64, session *imapSession, known map[string]bool) (*imap.SearchData, error) {
	// Build search criteria.
	criteria := timestampSearchCriteria(since, until)
	excludeDelivered(&criteria, known)

	// Search for messages.
	finish := session.wire.beginIO(ctx)
	session.listing.begin(src.limits.defaults().bytes)
	search := session.client.UIDSearch(&criteria, nil)
	searchData, err := search.Wait()
	err = errors.Join(err, session.listing.end())
	finish()
	if err != nil {
		return nil, fmt.Errorf("search: %w", joinTransportError(err, session.wire))
	}
	return searchData, nil
}

func (src *IMAPSource) fetchFolderBatches(ctx context.Context, folder string, msgChan chan<- *plugins.Message, since, until int64, sessions []*imapSession, uids []imap.UID, selectedUIDValidity uint32, scope string, progress *plugins.FetchProgress) error {
	const batchSize = 25
	batchCount := (len(uids) + batchSize - 1) / batchSize
	wantedSessions := min(src.maxConns, batchCount)
	activeSessions := append([]*imapSession(nil), sessions[:1]...)
	for len(activeSessions) < wantedSessions {
		session, err := src.openSession(ctx)
		if err != nil {
			for _, opened := range activeSessions[1:] {
				_ = opened.client.Close()
			}
			return fmt.Errorf("open IMAP connection pool: %w", err)
		}
		activeSessions = append(activeSessions, session)
	}
	defer func() {
		for _, session := range activeSessions[1:] {
			_ = session.client.Close()
		}
	}()
	workerCount := len(activeSessions)
	jobs := make(chan []imap.UID)
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, workerCount)
	var wg sync.WaitGroup
	var progressMu sync.Mutex
	for i := range workerCount {
		session := activeSessions[i]
		wg.Go(func() {
			src.runFetchWorker(workerCtx, ctx, cancel, session, folder, jobs, msgChan, since, until, selectedUIDValidity, scope, progress, &progressMu, errCh)
		})
	}
	go func() {
		defer close(jobs)
		for i := 0; i < len(uids); i += batchSize {
			end := min(i+batchSize, len(uids))
			select {
			case <-workerCtx.Done():
				return
			case jobs <- uids[i:end]:
			}
		}
	}()
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func (src *IMAPSource) runFetchWorker(workerCtx, ctx context.Context, cancel context.CancelFunc, session *imapSession, folder string, jobs <-chan []imap.UID, msgChan chan<- *plugins.Message, since, until int64, selectedUIDValidity uint32, scope string, progress *plugins.FetchProgress, progressMu *sync.Mutex, errCh chan<- error) {
	for batch := range jobs {
		if err := src.fetchOneBatch(workerCtx, ctx, session, folder, batch, msgChan, since, until, selectedUIDValidity, scope, progress, progressMu); err != nil {
			select {
			case errCh <- err:
			default:
			}
			cancel()
			return
		}
	}
}

func (src *IMAPSource) fetchOneBatch(workerCtx, ctx context.Context, session *imapSession, folder string, batch []imap.UID, msgChan chan<- *plugins.Message, since, until int64, selectedUIDValidity uint32, scope string, progress *plugins.FetchProgress, progressMu *sync.Mutex) error {
	ioDone := session.wire.beginIO(workerCtx)
	selected, err := session.client.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	ioDone()
	if err != nil {
		return fetchWorkerSelectionErr(workerCtx, session, folder, err)
	}
	if selected.UIDValidity != selectedUIDValidity {
		return fmt.Errorf("mailbox %q UIDVALIDITY changed during fetch", folder)
	}
	ioDone = session.wire.beginIO(workerCtx)
	err = src.fetchBatch(workerCtx, session, folder, batch, msgChan, since, until, scope, func() {
		progressMu.Lock()
		progress.Fetched++
		plugins.ReportFetchProgress(ctx, *progress)
		progressMu.Unlock()
	})
	ioDone()
	if err != nil {
		return fetchWorkerBatchErr(workerCtx, session, err)
	}
	return nil
}

func (src *IMAPSource) fetchBatch(ctx context.Context, session *imapSession, folder string, uids []imap.UID, msgChan chan<- *plugins.Message, since, until int64, scope string, fetched func()) (err error) {
	client := session.client
	// Fetch RFC822 for each UID.
	var seqSet imap.UIDSet
	for _, uid := range uids {
		seqSet.AddNum(uid)
	}

	fetch := client.Fetch(seqSet, &imap.FetchOptions{
		BodySection:  []*imap.FetchItemBodySection{{}},
		Envelope:     true,
		InternalDate: true,
		Flags:        true,
	})

	closeFetch := true
	defer func() {
		if closeFetch {
			err = errors.Join(err, fetch.Close())
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msg := fetch.Next()
		if msg == nil {
			break
		}

		// Collect message data into buffer.
		buf, err := collectFetchMessage(msg, plugins.MessageLimit(src.maxMessageBytes), func() { _ = session.wire.Close() })
		if err != nil {
			// A truncated literal releases the IMAP decoder before this goroutine
			// can read again. Closing the command would race that decoder; the
			// caller closes this session after the failed batch. Oversized literals
			// instead close the transport and discard each item exactly once, so
			// the command can safely release any remaining decoder waiters.
			closeFetch = errors.Is(err, plugins.ErrMessageTooLarge)
			return fmt.Errorf("collect message: %w", err)
		}
		fetched()
		matches, err := matchesTimestampBounds(buf.InternalDate, since, until)
		if err != nil {
			return err
		}
		if !matches {
			continue
		}

		// Get RFC822 body.
		section := &imap.FetchItemBodySection{}
		raw := buf.FindBodySection(section)
		if raw == nil {
			continue
		}
		pluginsMsg := buildFetchedMessage(buf, folder, scope, raw)
		select {
		case msgChan <- pluginsMsg:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

func (src *IMAPSource) closeLocked() error {
	client := src.client
	src.client = nil
	src.wire = nil
	src.connected = false
	if client != nil {
		return client.Close()
	}
	return nil
}

func (src *IMAPSource) connectLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if src.connected {
		select {
		case <-src.client.Closed():
			src.connected = false
		default:
			return nil
		}
	}

	var session *imapSession
	err := retryConnect(ctx, src.connectRetry, src.name, func(ctx context.Context) error {
		var err error
		session, err = src.openSession(ctx)
		return err
	})
	if err != nil {
		return err
	}
	src.listing = session.listing
	src.client = session.client
	src.wire = session.wire
	src.connected = true
	return nil
}

func (src *IMAPSource) openSession(ctx context.Context) (*imapSession, error) {
	addr := net.JoinHostPort(src.host, strconv.Itoa(src.port))
	dialer := &net.Dialer{Timeout: src.connTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	wire := &deadlineConn{Conn: raw, timeout: src.readTimeout}
	finish := wire.beginIO(ctx)
	defer finish()
	listing := &listingBudgetConn{Conn: wire}
	tlsConfig := &tls.Config{ServerName: src.host, MinVersion: tls.VersionTLS12}
	opts := &imapclient.Options{TLSConfig: tlsConfig}
	var client *imapclient.Client
	switch {
	case src.useTLS:
		secure := tls.Client(listing, tlsConfig)
		if err := secure.HandshakeContext(ctx); err != nil {
			return nil, errors.Join(fmt.Errorf("establish TLS: %w", err), raw.Close())
		}
		client = imapclient.New(secure, opts)
	case src.startTLS:
		client, err = imapclient.NewStartTLS(listing, opts)
		if err != nil {
			err = joinTransportError(err, wire)
		}
	default:
		client = imapclient.New(listing, opts)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("establish TLS: %w", err), raw.Close())
	}
	// The client library reports EOF/reset before the greeting as plain text;
	// joining the recorded transport error keeps it classifiable as transient.
	if err := client.WaitGreeting(); err != nil {
		err = joinTransportError(err, wire)
		return nil, errors.Join(fmt.Errorf("greeting: %w", err), client.Close())
	}
	if err := src.authenticate(ctx, client, wire); err != nil {
		return nil, err
	}
	return &imapSession{client: client, wire: wire, listing: listing}, nil
}

func (src *IMAPSource) authenticate(ctx context.Context, client *imapclient.Client, wire *deadlineConn) error {
	if src.tokenSource != nil {
		return src.authenticateOAuth2(ctx, client, wire)
	}
	if err := client.Login(src.username, src.password).Wait(); err != nil {
		err = errors.Join(fmt.Errorf("login: %w", joinTransportError(err, wire)), client.Close())
		if isIMAPRejection(err) {
			err = permanentConnectError(err)
		}
		return err
	}
	return nil
}

func (src *IMAPSource) authenticateOAuth2(ctx context.Context, client *imapclient.Client, wire *deadlineConn) error {
	token, err := src.tokenSource(ctx)
	if err != nil {
		return permanentConnectError(errors.Join(fmt.Errorf("get OAuth2 access token: %w", err), client.Close()))
	}
	if err := client.Authenticate(auth.NewXOAUTH2(src.username, token)); err != nil {
		return src.oauth2AuthError(err, client, wire)
	}
	return nil
}

func (src *IMAPSource) oauth2AuthError(err error, client *imapclient.Client, wire *deadlineConn) error {
	if err = joinTransportError(err, wire); !isIMAPRejection(err) {
		return errors.Join(fmt.Errorf("IMAP OAuth2 authentication: %w", err), client.Close())
	}
	return permanentConnectError(errors.Join(fmt.Errorf("IMAP OAuth2 authentication failed for mailbox %q; the server rejected access but did not establish the cause. Check that device/browser login used the account owning this mailbox, IMAP is enabled, and delegated IMAP permission was granted; then run mailvault auth login --source %q again", src.username, src.name), client.Close()))
}

// extractMessageIDFromEnvelope extracts Message-ID from IMAP envelope.
func extractMessageIDFromEnvelope(env *imap.Envelope) string {
	if env == nil || env.MessageID == "" {
		return ""
	}
	// Envelope.MessageID already has < > stripped by go-imap.
	return env.MessageID
}
