package source

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/zekihan/mailvault/internal/auth"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/config"
	"sync"
)

type pop3Identity struct {
	ID        int
	UID       string
	sharedUID bool
}

const defaultPOP3Port = 995

// POP3Source implements the Source interface for POP3.
type POP3Source struct {
	limits          listingLimits
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
	conn            *pop3Protocol
	wire            *deadlineConn
	tlsConfig       *tls.Config
	tokenSource     func(context.Context) (string, error)
	connected       bool
	mu              sync.Mutex
}

type parsedPOP3Config struct {
	limits           listingLimits
	maxMessageBytes  int64
	startTLS, useTLS bool
	host             string
	port             int
	username         string
	passwordRef      string
	tokenSource      func(context.Context) (string, error)
	folders          []string
	connTimeout      time.Duration
	readTimeout      time.Duration
}

func NewPOP3Source(configMap map[string]any) (plugins.Source, error) {
	parsed, err := parsePOP3Config(configMap)
	if err != nil {
		return nil, err
	}
	return &POP3Source{
		limits:          parsed.limits,
		name:            "pop3:" + parsed.host, // Will be overridden by registry
		host:            parsed.host,
		port:            parsed.port,
		username:        parsed.username,
		password:        parsed.passwordRef,
		useTLS:          parsed.useTLS,
		startTLS:        parsed.startTLS,
		tokenSource:     parsed.tokenSource,
		folders:         parsed.folders,
		connTimeout:     parsed.connTimeout,
		readTimeout:     parsed.readTimeout,
		maxMessageBytes: parsed.maxMessageBytes,
	}, nil
}

func parsePOP3Config(configMap map[string]any) (parsedPOP3Config, error) {
	var parsed parsedPOP3Config
	if err := parsePOP3ConfigValues(configMap, &parsed); err != nil {
		return parsedPOP3Config{}, err
	}
	return parsed, nil
}

func parsePOP3ConfigValues(configMap map[string]any, parsed *parsedPOP3Config) error {
	var err error
	parsed.limits, err = parseListingLimits(configMap)
	if err != nil {
		return err
	}
	parsed.maxMessageBytes, err = config.ParseMessageLimit(configMap)
	if err != nil {
		return err
	}
	if parsed.startTLS, err = parseIMAPBool(configMap, config.SourceConfigStartTLS, false); err != nil {
		return err
	}
	if parsed.host, err = parseIMAPString(configMap, config.SourceConfigHost); err != nil {
		return err
	}
	if parsed.port, err = parseIMAPInt(configMap, config.SourceConfigPort, defaultPOP3Port); err != nil {
		return err
	}
	return parsePOP3ConfigCredentials(configMap, parsed)
}

func parsePOP3ConfigCredentials(configMap map[string]any, parsed *parsedPOP3Config) error {
	var err error
	if parsed.username, err = parseIMAPString(configMap, config.SourceConfigUsername); err != nil {
		return err
	}
	if parsed.passwordRef, err = parseIMAPString(configMap, config.SourceConfigPasswordRef); err != nil {
		return err
	}
	if parsed.useTLS, err = parseIMAPBool(configMap, config.SourceConfigUseTLS, true); err != nil {
		return err
	}
	if parsed.connTimeout, err = parseIMAPDuration(configMap, config.SourceConfigConnectionTimeout, defaultConnectionTimeout); err != nil {
		return err
	}
	if parsed.readTimeout, err = parseIMAPDuration(configMap, config.SourceConfigReadTimeout, defaultReadTimeout); err != nil {
		return err
	}
	if v, ok := configMap["oauth2_token_provider"].(func(context.Context) (string, error)); ok {
		parsed.tokenSource = v
	}
	return parsePOP3ConfigValidate(configMap, parsed)
}

func parsePOP3ConfigValidate(configMap map[string]any, parsed *parsedPOP3Config) error {
	if parsed.host == "" {
		return errors.New("host is required")
	}
	if parsed.username == "" {
		return errors.New("username is required")
	}
	if parsed.passwordRef == "" && parsed.tokenSource == nil {
		return errors.New("password_ref is required")
	}
	if parsed.startTLS && parsed.useTLS {
		return errors.New("POP3 start_tls and use_tls cannot both be enabled")
	}
	if parsed.tokenSource != nil && !parsed.useTLS && !parsed.startTLS {
		return errors.New("POP3 OAuth2 requires TLS; enable use_tls or start_tls")
	}
	// POP3 doesn't support folders, but we keep the config for consistency.
	folders, err := config.ParseStringSlice(configMap, config.SourceConfigFolders)
	if err != nil {
		return fmt.Errorf("%s: %w", config.SourceConfigFolders, err)
	}
	if len(folders) == 0 {
		folders = []string{"INBOX"}
	}
	parsed.folders = folders
	return nil
}

// SetName sets the source name (called by registry).
func (src *POP3Source) SetName(name string) {
	src.name = name
}

func (src *POP3Source) Name() string {
	return src.name
}

func (src *POP3Source) Type() string {
	return "pop3"
}

func (src *POP3Source) Connect(ctx context.Context) error {
	src.mu.Lock()
	defer src.mu.Unlock()
	return src.connectLocked(ctx)
}

func (src *POP3Source) ListFolders(ctx context.Context) ([]string, error) {
	// POP3 doesn't have folders, return INBOX only.
	return src.folders, nil
}

func (src *POP3Source) FetchMessages(ctx context.Context, folders []string, since, until int64) (<-chan *plugins.Message, <-chan error) {
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
			finish := src.wire.beginIO(ctx)
			defer finish()
			if err := src.fetchFolder(ctx, "INBOX", output, since, until); err != nil {
				src.connected = false
				_ = src.wire.Close()
				src.conn = nil
				return fmt.Errorf("fetch folder: %w", err)
			}
			return nil
		})
		if err != nil {
			errChan <- err
		}
	}()

	return msgChan, errChan
}

func isPOP3UIDLSetupError(err error) bool {
	var netErr net.Error
	var numErr *strconv.NumError
	return errors.Is(err, errListingLimit) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &netErr) || errors.As(err, &numErr) || !isUIDLUnsupported(err)
}

func pop3PendingIdentities(identities []pop3Identity, known map[string]bool) []pop3Identity {
	pending := identities[:0]
	for _, item := range identities {
		if item.sharedUID || !known[item.UID] {
			pending = append(pending, item)
		}
	}
	return pending
}

func pop3FetchIdentity(index int, scope string, pending []pop3Identity) (pop3Identity, string) {
	item := pop3Identity{ID: index + 1}
	if scope != "" {
		item = pending[index]
	}
	messageScope := scope
	if item.sharedUID || scope == "" {
		messageScope = ""
		item.UID = fmt.Sprintf("sequence:%d", item.ID)
	}
	return item, messageScope
}

func pop3MessageDate(raw []byte, uid string, since, until int64) (time.Time, error) {
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		if since != 0 || until != 0 {
			return time.Time{}, fmt.Errorf("parse Date header for UIDL %q: %w", uid, err)
		}
	}
	var messageDate time.Time
	if parsed != nil {
		messageDate, err = parsed.Header.Date()
	}
	if (since != 0 || until != 0) && (err != nil || messageDate.IsZero()) {
		return time.Time{}, fmt.Errorf("POP3 message %q has no valid Date header required for date filtering", uid)
	}
	return messageDate, nil
}

func pop3MessageOutsideBounds(messageDate time.Time, since, until int64) bool {
	return (since != 0 && messageDate.Unix() < since) || (until != 0 && messageDate.Unix() >= until)
}

func buildPOP3Message(folder string, raw []byte, item pop3Identity, messageScope string, messageDate time.Time) *plugins.Message {
	// Parse headers for Message-ID.
	messageID := dedup.ExtractMessageID(raw)
	// Compute content hash.
	contentHash := dedup.ComputeContentHash(raw)
	// Internal date - POP3 doesn't provide this reliably.
	internalDate := time.Now().Format(time.RFC3339)
	if !messageDate.IsZero() {
		internalDate = messageDate.Format(time.RFC3339)
	}
	return &plugins.Message{
		UIDScope:     messageScope,
		UID:          item.UID,
		Folder:       folder,
		Raw:          raw,
		MessageID:    messageID,
		ContentHash:  contentHash,
		Size:         int64(len(raw)),
		InternalDate: internalDate,
	}
}

func (src *POP3Source) Close() error {
	src.mu.Lock()
	defer src.mu.Unlock()

	var err error
	if src.conn != nil {
		// Send QUIT command.
		finish := src.wire.beginIO(context.Background())
		defer finish()
		err = src.conn.Quit()
		src.conn = nil
	}
	src.connected = false
	return err
}

func (src *POP3Source) fetchFolder(ctx context.Context, folder string, msgChan chan<- *plugins.Message, since, until int64) error {
	progress := plugins.FetchProgress{Folder: folder, Total: -1, Stage: "counting"}
	plugins.ReportFetchProgress(ctx, progress)

	count, err := src.statPOP3Count()
	if err != nil {
		return err
	}
	scope, identities, err := src.loadPOP3Identities(ctx, count, &progress)
	if err != nil {
		return err
	}
	pending, _, err := src.filterPOP3Known(ctx, folder, scope, identities, &progress, count)
	if err != nil {
		return err
	}
	if progress.Total == 0 {
		progress.Stage = "finished"
		plugins.ReportFetchProgress(ctx, progress)
		return nil
	}
	return src.fetchPOP3Messages(ctx, folder, msgChan, since, until, scope, pending, &progress)
}

func (src *POP3Source) statPOP3Count() (int, error) {
	// Get message count.
	count, _, err := src.conn.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat: %w", err)
	}
	if count < 0 || count > src.limits.defaults().items {
		return 0, fmt.Errorf("POP3 protocol error: STAT count %d outside 0..%d", count, src.limits.defaults().items)
	}
	return count, nil
}

func (src *POP3Source) loadPOP3Identities(ctx context.Context, count int, progress *plugins.FetchProgress) (string, []pop3Identity, error) {
	scope := dedup.ComputeContentHash([]byte(fmt.Sprintf("pop3:%s:%d\x00%s", src.host, src.port, src.username)))
	identities, err := readPOP3UIDLs(src.conn, count, src.limits.defaults())
	if err != nil {
		if isPOP3UIDLSetupError(err) {
			return "", nil, fmt.Errorf("UIDL: %w", err)
		}
		progress.Stage = "UIDL unavailable; full fetch"
		plugins.ReportFetchProgress(ctx, *progress)
		// Fallback identities are generated lazily below.
		return "", nil, nil
	}
	if len(identities) != count {
		return "", nil, fmt.Errorf("UIDL returned %d messages, STAT reported %d", len(identities), count)
	}
	return scope, identities, nil
}

func (src *POP3Source) filterPOP3Known(ctx context.Context, folder, scope string, identities []pop3Identity, progress *plugins.FetchProgress, count int) ([]pop3Identity, map[string]bool, error) {
	var known map[string]bool
	if scope != "" {
		var err error
		known, err = plugins.KnownUIDs(ctx, scope, "INBOX")
		if err != nil {
			return nil, nil, fmt.Errorf("load incremental state: %w", err)
		}
		known = skipStreamedUIDs(ctx, scope, folder, known)
	}
	pending := pop3PendingIdentities(identities, known)
	progress.Total = len(pending)
	if scope == "" {
		progress.Total = count
	}
	progress.Stage = "fetching"
	plugins.ReportFetchProgress(ctx, *progress)
	return pending, known, nil
}

func (src *POP3Source) fetchPOP3Messages(ctx context.Context, folder string, msgChan chan<- *plugins.Message, since, until int64, scope string, pending []pop3Identity, progress *plugins.FetchProgress) error {
	// Fetch each message.
	for i := range progress.Total {
		item, messageScope := pop3FetchIdentity(i, scope, pending)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := src.fetchOnePOP3Message(ctx, folder, msgChan, since, until, item, messageScope, progress); err != nil {
			return err
		}
	}
	progress.Stage = "finished"
	plugins.ReportFetchProgress(ctx, *progress)
	return nil
}

func (src *POP3Source) fetchOnePOP3Message(ctx context.Context, folder string, msgChan chan<- *plugins.Message, since, until int64, item pop3Identity, messageScope string, progress *plugins.FetchProgress) error {
	// RETR retrieves the full message.
	raw, err := src.conn.retrieve(item.ID, src.maxMessageBytes)
	if err != nil {
		return fmt.Errorf("retr message %d: %w", item.ID, err)
	}
	progress.Fetched++
	plugins.ReportFetchProgress(ctx, *progress)
	messageDate, err := pop3MessageDate(raw, item.UID, since, until)
	if err != nil {
		return err
	}
	if pop3MessageOutsideBounds(messageDate, since, until) {
		return nil
	}
	pluginsMsg := buildPOP3Message(folder, raw, item, messageScope, messageDate)
	select {
	case msgChan <- pluginsMsg:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (src *POP3Source) connectLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if src.connected {
		return nil
	}

	var wire *deadlineConn
	var upgraded *switchConn
	dialer := &net.Dialer{Timeout: src.connTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(src.host, strconv.Itoa(src.port)))
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	wire = &deadlineConn{Conn: raw, timeout: src.readTimeout}
	finish := wire.beginIO(ctx)
	defer finish()
	upgraded = &switchConn{Conn: wire}
	if src.useTLS {
		secure := tls.Client(wire, src.tlsOptions())
		if err := secure.HandshakeContext(ctx); err != nil {
			return errors.Join(err, wire.Close())
		}
		upgraded.set(secure)
	}
	conn, err := newPOP3Protocol(upgraded, src.limits.defaults())
	if err != nil {
		return errors.Join(fmt.Errorf("connect: %w", err), wire.Close())
	}

	if src.startTLS {
		if err := conn.startTLS(ctx, src.tlsOptions()); err != nil {
			return errors.Join(fmt.Errorf("STARTTLS: %w", err), wire.Close())
		}
	}
	if src.tokenSource != nil {
		token, err := src.tokenSource(ctx)
		if err != nil {
			return errors.Join(fmt.Errorf("get OAuth2 access token: %w", err), wire.Close())
		}
		mechanism, response, err := auth.NewXOAUTH2(src.username, token).Start()
		if err != nil {
			return errors.Join(fmt.Errorf("build OAuth2 auth: %w", err), wire.Close())
		}
		if err := pop3AuthenticateXOAUTH2(conn, mechanism, base64.StdEncoding.EncodeToString(response)); err != nil {
			return errors.Join(errors.New("POP3 OAuth2 authentication failed"), wire.Close())
		}
	} else if err := conn.Auth(src.username, src.password); err != nil {
		return errors.Join(fmt.Errorf("auth: %w", err), wire.Close())
	}

	src.conn = conn
	src.wire = wire
	src.connected = true
	return nil
}

func (src *POP3Source) tlsOptions() *tls.Config {
	if src.tlsConfig == nil {
		return &tls.Config{ServerName: src.host, MinVersion: tls.VersionTLS12}
	}
	options := src.tlsConfig.Clone()
	options.ServerName = src.host
	if options.MinVersion < tls.VersionTLS12 {
		options.MinVersion = tls.VersionTLS12
	}
	return options
}

func isUIDLUnsupported(err error) bool {
	message := strings.ToLower(err.Error())
	if strings.HasPrefix(message, "unknown response:") {
		return false
	}
	return strings.Contains(message, "not support") || strings.Contains(message, "unsupported") || strings.Contains(message, "unknown command") || strings.Contains(message, "not implemented")
}

func pop3AuthenticateXOAUTH2(conn *pop3Protocol, mechanism, response string) error {
	if err := conn.Send("AUTH " + mechanism + " " + response); err != nil {
		return err
	}
	_, err := conn.ReadOne()
	if err == nil {
		return nil
	}
	if strings.HasPrefix(err.Error(), "unknown response: +") {
		if err := conn.Send(""); err != nil {
			return err
		}
		_, err = conn.ReadOne()
		return err
	}
	return err
}
