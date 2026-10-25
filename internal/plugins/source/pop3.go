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
	ID  int
	UID string
}

// POP3Source implements the Source interface for POP3.
type POP3Source struct {
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

func NewPOP3Source(configMap map[string]interface{}) (plugins.Source, error) {
	maxMessageBytes, err := config.ParseMessageLimit(configMap)
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
		name:            "pop3:" + host, // Will be overridden by registry
		host:            host,
		port:            port,
		username:        username,
		password:        passwordRef,
		useTLS:          useTLS,
		startTLS:        startTLS,
		tokenSource:     tokenSource,
		folders:         folders,
		connTimeout:     connTimeout,
		readTimeout:     readTimeout,
		maxMessageBytes: maxMessageBytes,
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
	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(s.host, strconv.Itoa(s.port)))
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	wire = &deadlineConn{Conn: raw, timeout: s.readTimeout}
	finish = wire.beginIO(ctx)
	upgraded = &switchConn{Conn: wire}
	if s.useTLS {
		secure := tls.Client(wire, s.tlsOptions())
		if err := secure.HandshakeContext(ctx); err != nil {
			return errors.Join(err, wire.Close())
		}
		upgraded.set(secure)
	}
	conn, err := newPOP3Protocol(upgraded)
	if err != nil {
		return errors.Join(fmt.Errorf("connect: %w", err), wire.Close())
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
	msgChan := make(chan *plugins.Message)
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
	count, _, err := s.conn.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}

	scope := dedup.ComputeContentHash([]byte(fmt.Sprintf("pop3:%s:%d\x00%s", s.host, s.port, s.username)))
	identities, err := readPOP3UIDLs(s.conn)
	if err != nil {
		var netErr net.Error
		var numErr *strconv.NumError
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &netErr) || errors.As(err, &numErr) || !isUIDLUnsupported(err) {
			return fmt.Errorf("UIDL: %w", err)
		}
		progress.Stage = "UIDL unavailable; full fetch"
		plugins.ReportFetchProgress(ctx, progress)
		scope = ""
		identities = make([]pop3Identity, count)
		for i := range identities {
			identities[i] = pop3Identity{ID: i + 1, UID: fmt.Sprintf("sequence:%d", i+1)}
		}
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
	seen := make(map[string]bool, len(identities))
	seenIDs := make(map[int]bool, len(identities))
	pending := make([]pop3Identity, 0, len(identities))
	for _, item := range identities {
		if scope != "" {
			item.UID = strings.TrimSpace(item.UID)
			if item.ID <= 0 || item.ID > count || item.UID == "" || seen[item.UID] || seenIDs[item.ID] {
				return fmt.Errorf("invalid or duplicate UIDL result for message %d", item.ID)
			}
			seen[item.UID] = true
			seenIDs[item.ID] = true
		}
		if !known[item.UID] {
			pending = append(pending, item)
		}
	}
	progress.Total = len(pending)
	progress.Stage = "fetching"
	plugins.ReportFetchProgress(ctx, progress)
	if len(pending) == 0 {
		progress.Stage = "finished"
		plugins.ReportFetchProgress(ctx, progress)
		return nil
	}

	// Fetch each message
	for _, item := range pending {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// RETR retrieves the full message
		raw, err := s.conn.retrieve(item.ID, s.maxMessageBytes)
		if err != nil {
			return fmt.Errorf("retr message %d: %w", item.ID, err)
		}

		progress.Fetched++
		plugins.ReportFetchProgress(ctx, progress)

		parsed, err := mail.ReadMessage(bytes.NewReader(raw))
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

func pop3AuthenticateXOAUTH2(conn *pop3Protocol, mechanism, response string) error {
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

func readPOP3UIDLs(conn *pop3Protocol) ([]pop3Identity, error) {
	response, err := conn.Cmd("UIDL", true)
	if err != nil {
		return nil, err
	}
	var identities []pop3Identity
	for _, line := range bytes.Split(response.Bytes(), []byte("\r\n")) {
		fields := bytes.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("malformed UIDL row with %d fields", len(fields))
		}
		id, err := strconv.Atoi(string(fields[0]))
		if err != nil {
			return nil, fmt.Errorf("parse UIDL message number: %w", err)
		}
		identities = append(identities, pop3Identity{ID: id, UID: string(fields[1])})
	}
	return identities, nil
}
