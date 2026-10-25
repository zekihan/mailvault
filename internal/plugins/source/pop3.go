package source

import (
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

	"github.com/knadh/go-pop3"
	"github.com/zekihan/mailvault/internal/auth"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/config"
	"sync"
)

// POP3Source implements the Source interface for POP3.
type POP3Source struct {
	limits      listingLimits
	stream      *pop3LineConn
	listing     *listingBudgetConn
	name        string
	host        string
	port        int
	username    string
	password    string
	useTLS      bool
	startTLS    bool
	folders     []string
	connTimeout time.Duration
	readTimeout time.Duration
	client      *pop3.Client
	conn        *pop3.Conn
	wire        *deadlineConn
	tlsConfig   *tls.Config
	tokenSource func(context.Context) (string, error)
	connected   bool
	mu          sync.Mutex
}

func NewPOP3Source(configMap map[string]interface{}) (plugins.Source, error) {
	limits, err := parseListingLimits(configMap)
	if err != nil {
		return nil, err
	}
	startTLS, err := config.ParseBool(configMap, config.SourceConfigStartTLS, false)
	if err != nil {
		return nil, err
	}
	host, _ := config.ParseString(configMap, config.SourceConfigHost, "")
	port, _ := config.ParseInt(configMap, config.SourceConfigPort, 995)
	username, _ := config.ParseString(configMap, config.SourceConfigUsername, "")
	passwordRef, _ := config.ParseString(configMap, config.SourceConfigPasswordRef, "")
	useTLS, _ := config.ParseBool(configMap, config.SourceConfigUseTLS, true)
	connTimeout, _ := config.ParseDuration(configMap, config.SourceConfigConnectionTimeout, 30*time.Second)
	readTimeout, _ := config.ParseDuration(configMap, config.SourceConfigReadTimeout, 60*time.Second)
	tokenSource, _ := configMap["oauth2_token_provider"].(func(context.Context) (string, error))

	if host == "" {
		return nil, errors.New("host is required")
	}
	if username == "" {
		return nil, errors.New("username is required")
	}
	if passwordRef == "" && tokenSource == nil {
		return nil, errors.New("password_ref is required")
	}
	if startTLS && useTLS {
		return nil, errors.New("POP3 start_tls and use_tls cannot both be enabled")
	}
	if tokenSource != nil && !useTLS && !startTLS {
		return nil, errors.New("POP3 OAuth2 requires TLS; enable use_tls or start_tls")
	}

	// POP3 doesn't support folders, but we keep the config for consistency
	folders, _ := config.ParseStringSlice(configMap, config.SourceConfigFolders)
	if len(folders) == 0 {
		folders = []string{"INBOX"}
	}

	return &POP3Source{
		limits:      limits,
		name:        "pop3:" + host, // Will be overridden by registry
		host:        host,
		port:        port,
		username:    username,
		password:    passwordRef,
		useTLS:      useTLS,
		startTLS:    startTLS,
		tokenSource: tokenSource,
		folders:     folders,
		connTimeout: connTimeout,
		readTimeout: readTimeout,
	}, nil
}

// SetName sets the source name (called by registry).
func (s *POP3Source) SetName(name string) {
	s.name = name
}

func (s *POP3Source) Name() string {
	return s.name
}

func (s *POP3Source) Type() string {
	return "pop3"
}

func (s *POP3Source) Connect(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connectLocked(ctx)
}

func (s *POP3Source) connectLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.connected {
		return nil
	}

	var wire *deadlineConn
	var upgraded *switchConn
	finish := func() {}
	defer func() { finish() }()
	dialer := &net.Dialer{Timeout: s.connTimeout}
	opt := pop3.Opt{
		Host:        s.host,
		Port:        s.port,
		DialTimeout: s.connTimeout,
		TLSEnabled:  false,
		Dialer: dialFunc(func(network, address string) (net.Conn, error) {
			raw, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			wire = &deadlineConn{Conn: raw, timeout: s.readTimeout}
			finish = wire.beginIO(ctx)
			upgraded = &switchConn{Conn: wire}
			if s.useTLS {
				tlsConn := tls.Client(wire, s.tlsOptions())
				if err := tlsConn.HandshakeContext(ctx); err != nil {
					_ = wire.Close()
					return nil, fmt.Errorf("TLS handshake: %w", err)
				}
				upgraded.set(tlsConn)
			}
			s.listing = &listingBudgetConn{Conn: upgraded}
			s.stream = newPOP3LineConn(s.listing)
			return s.stream, nil
		}),
	}

	s.client = pop3.New(opt)

	// Create connection
	conn, err := s.client.NewConn()
	if err != nil {
		if wire != nil {
			err = errors.Join(err, wire.Close())
		}
		return fmt.Errorf("connect: %w", err)
	}

	if s.startTLS {
		if _, err := conn.Cmd("STLS", false); err != nil {
			return errors.Join(fmt.Errorf("STARTTLS: %w", err), wire.Close())
		}
		tlsConn := tls.Client(wire, s.tlsOptions())
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return errors.Join(fmt.Errorf("STARTTLS handshake: %w", err), wire.Close())
		}
		upgraded.set(tlsConn)
	}
	if s.tokenSource != nil {
		token, err := s.tokenSource(ctx)
		if err != nil {
			return errors.Join(fmt.Errorf("get OAuth2 access token: %w", err), wire.Close())
		}
		mechanism, response, err := auth.NewXOAUTH2(s.username, token).Start()
		if err != nil {
			return errors.Join(fmt.Errorf("build OAuth2 auth: %w", err), wire.Close())
		}
		if err := pop3AuthenticateXOAUTH2(conn, mechanism, base64.StdEncoding.EncodeToString(response)); err != nil {
			return errors.Join(errors.New("POP3 OAuth2 authentication failed"), wire.Close())
		}
	} else if err := conn.Auth(s.username, s.password); err != nil {
		return errors.Join(fmt.Errorf("auth: %w", err), wire.Close())
	}

	s.conn = conn
	s.wire = wire
	s.connected = true
	return nil
}

func (s *POP3Source) tlsOptions() *tls.Config {
	if s.tlsConfig == nil {
		return &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}
	}
	options := s.tlsConfig.Clone()
	options.ServerName = s.host
	if options.MinVersion < tls.VersionTLS12 {
		options.MinVersion = tls.VersionTLS12
	}
	return options
}

func (s *POP3Source) ListFolders(ctx context.Context) ([]string, error) {
	// POP3 doesn't have folders, return INBOX only
	return s.folders, nil
}

func (s *POP3Source) FetchMessages(ctx context.Context, folders []string, since, until int64) (<-chan *plugins.Message, <-chan error) {
	msgChan := make(chan *plugins.Message, 100)
	errChan := make(chan error, 1)

	go func() {
		defer close(msgChan)
		defer close(errChan)
		s.mu.Lock()
		defer s.mu.Unlock()

		err := retryFetch(ctx, msgChan, func(ctx context.Context, output chan<- *plugins.Message) error {
			if err := s.connectLocked(ctx); err != nil {
				return err
			}
			finish := s.wire.beginIO(ctx)
			defer finish()
			if err := s.fetchFolder(ctx, "INBOX", output, since, until); err != nil {
				s.connected = false
				_ = s.wire.Close()
				s.conn = nil
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

func (s *POP3Source) fetchFolder(ctx context.Context, folder string, msgChan chan<- *plugins.Message, since, until int64) error {
	progress := plugins.FetchProgress{Folder: folder, Total: -1, Stage: "counting"}
	plugins.ReportFetchProgress(ctx, progress)

	// Get message count
	s.listing.begin(s.limits.defaults().bytes)
	count, _, err := s.conn.Stat()
	err = errors.Join(err, s.listing.end())
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}

	if count < 0 || count > s.limits.defaults().items {
		return fmt.Errorf("POP3 protocol error: STAT count %d outside 0..%d", count, s.limits.defaults().items)
	}

	scope := dedup.ComputeContentHash([]byte(fmt.Sprintf("pop3:%s:%d\x00%s", s.host, s.port, s.username)))
	s.listing.begin(s.limits.defaults().bytes)
	identities, err := readPOP3UIDLs(s.conn, s.stream, count, s.limits.defaults())
	err = errors.Join(err, s.listing.end())
	if err != nil {
		var netErr net.Error
		var numErr *strconv.NumError
		if errors.Is(err, errListingLimit) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &netErr) || errors.As(err, &numErr) || !isUIDLUnsupported(err) {
			return fmt.Errorf("UIDL: %w", err)
		}
		progress.Stage = "UIDL unavailable; full fetch"
		plugins.ReportFetchProgress(ctx, progress)
		scope = ""
		// Fallback identities are generated lazily below.
	} else if len(identities) != count {
		return fmt.Errorf("UIDL returned %d messages, STAT reported %d", len(identities), count)
	}
	var known map[string]bool
	if scope != "" {
		known, err = plugins.KnownUIDs(ctx, scope, "INBOX")
		if err != nil {
			return fmt.Errorf("load incremental state: %w", err)
		}
	}
	pending := identities[:0]
	for _, item := range identities {
		if !known[item.UID] {
			pending = append(pending, item)
		}
	}
	progress.Total = len(pending)
	if scope == "" {
		progress.Total = count
	}
	progress.Stage = "fetching"
	plugins.ReportFetchProgress(ctx, progress)
	if progress.Total == 0 {
		progress.Stage = "finished"
		plugins.ReportFetchProgress(ctx, progress)
		return nil
	}

	// Fetch each message
	for i := 0; i < progress.Total; i++ {
		var item pop3.MessageID
		if scope == "" {
			item = pop3.MessageID{ID: i + 1, UID: fmt.Sprintf("sequence:%d", i+1)}
		} else {
			item = pending[i]
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// RETR retrieves the full message
		buf, err := s.conn.RetrRaw(item.ID)
		if err != nil {
			return fmt.Errorf("retr message %d: %w", item.ID, err)
		}

		raw := buf.Bytes()
		progress.Fetched++
		plugins.ReportFetchProgress(ctx, progress)

		parsed, err := mail.ReadMessage(strings.NewReader(string(raw)))
		if err != nil {
			if since != 0 || until != 0 {
				return fmt.Errorf("parse Date header for UIDL %q: %w", item.UID, err)
			}
		}
		var messageDate time.Time
		if parsed != nil {
			messageDate, err = parsed.Header.Date()
		}
		if (since != 0 || until != 0) && (err != nil || messageDate.IsZero()) {
			return fmt.Errorf("POP3 message %q has no valid Date header required for date filtering", item.UID)
		}
		if (since != 0 && messageDate.Unix() < since) || (until != 0 && messageDate.Unix() >= until) {
			continue
		}

		// Parse headers for Message-ID
		messageID := dedup.ExtractMessageID(raw)
		if messageID == "" {
			// Try to extract from common headers
			messageID = extractMessageIDFromRaw(raw)
		}

		// Compute content hash
		contentHash := dedup.ComputeContentHash(raw)

		// Internal date - POP3 doesn't provide this reliably
		internalDate := time.Now().Format(time.RFC3339)
		if !messageDate.IsZero() {
			internalDate = messageDate.Format(time.RFC3339)
		}

		pluginsMsg := &plugins.Message{
			UIDScope:     scope,
			UID:          item.UID,
			Folder:       folder,
			Raw:          raw,
			MessageID:    messageID,
			ContentHash:  contentHash,
			Size:         int64(len(raw)),
			InternalDate: internalDate,
		}

		select {
		case msgChan <- pluginsMsg:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	progress.Stage = "finished"
	plugins.ReportFetchProgress(ctx, progress)
	return nil
}

func (s *POP3Source) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var err error
	if s.conn != nil {
		// Send QUIT command
		finish := s.wire.beginIO(context.Background())
		defer finish()
		err = s.conn.Quit()
		s.conn = nil
	}
	s.client = nil
	s.connected = false
	return err
}

func isUIDLUnsupported(err error) bool {
	message := strings.ToLower(err.Error())
	if strings.HasPrefix(message, "unknown response:") {
		return false
	}
	return strings.Contains(message, "not support") || strings.Contains(message, "unsupported") || strings.Contains(message, "unknown command") || strings.Contains(message, "not implemented")
}

func pop3AuthenticateXOAUTH2(conn *pop3.Conn, mechanism, response string) error {
	if err := conn.Send("AUTH " + mechanism + " " + response); err != nil {
		return err
	}
	if _, err := conn.ReadOne(); err == nil {
		return nil
	} else if strings.HasPrefix(err.Error(), "unknown response: +") {
		if err := conn.Send(""); err != nil {
			return err
		}
		_, err = conn.ReadOne()
		return err
	} else {
		return err
	}
}

// extractMessageIDFromRaw tries to extract Message-ID from raw message more thoroughly.
func extractMessageIDFromRaw(raw []byte) string {
	// Simple parser - look for Message-ID header
	lines := strings.Split(string(raw), "\n")
	inHeader := true
	for _, line := range lines {
		if inHeader {
			if line == "" || line == "\r" {
				inHeader = false
				continue
			}
			lower := strings.ToLower(line)
			if strings.HasPrefix(lower, "message-id:") {
				value := strings.TrimSpace(line[len("Message-ID:"):])
				value = strings.TrimSpace(value)
				if strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">") {
					return value[1 : len(value)-1]
				}
				return value
			}
		}
	}
	return ""
}
