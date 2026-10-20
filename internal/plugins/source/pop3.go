package source

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/go-pop3"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/config"
	"sync"
)

// POP3Source implements the Source interface for POP3.
type POP3Source struct {
	name        string
	host        string
	port        int
	username    string
	password    string
	useTLS      bool
	folders     []string
	connTimeout time.Duration
	readTimeout time.Duration
	client      *pop3.Client
	conn        *pop3.Conn
	wire        *deadlineConn
	connected   bool
	mu          sync.Mutex
}

func NewPOP3Source(configMap map[string]interface{}) (plugins.Source, error) {
	startTLS, err := config.ParseBool(configMap, config.SourceConfigStartTLS, false)
	if err != nil {
		return nil, err
	}
	if startTLS {
		return nil, errors.New("POP3 STARTTLS is unsupported; use implicit TLS instead")
	}
	host, _ := config.ParseString(configMap, config.SourceConfigHost, "")
	port, _ := config.ParseInt(configMap, config.SourceConfigPort, 995)
	username, _ := config.ParseString(configMap, config.SourceConfigUsername, "")
	passwordRef, _ := config.ParseString(configMap, config.SourceConfigPasswordRef, "")
	useTLS, _ := config.ParseBool(configMap, config.SourceConfigUseTLS, true)
	connTimeout, _ := config.ParseDuration(configMap, config.SourceConfigConnectionTimeout, 30*time.Second)
	readTimeout, _ := config.ParseDuration(configMap, config.SourceConfigReadTimeout, 60*time.Second)

	if host == "" {
		return nil, errors.New("host is required")
	}
	if username == "" {
		return nil, errors.New("username is required")
	}
	if passwordRef == "" {
		return nil, errors.New("password_ref is required")
	}

	// POP3 doesn't support folders, but we keep the config for consistency
	folders, _ := config.ParseStringSlice(configMap, config.SourceConfigFolders)
	if len(folders) == 0 {
		folders = []string{"INBOX"}
	}

	return &POP3Source{
		name:        "pop3:" + host, // Will be overridden by registry
		host:        host,
		port:        port,
		username:    username,
		password:    passwordRef,
		useTLS:      useTLS,
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
	finish := func() {}
	defer func() { finish() }()
	dialer := &net.Dialer{Timeout: s.connTimeout}
	opt := pop3.Opt{
		Host:        s.host,
		Port:        s.port,
		DialTimeout: s.connTimeout,
		TLSEnabled:  s.useTLS,
		Dialer: dialFunc(func(network, address string) (net.Conn, error) {
			raw, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			wire = &deadlineConn{Conn: raw, timeout: s.readTimeout}
			finish = wire.beginIO(ctx)
			return wire, nil
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

	// Authenticate
	if err := conn.Auth(s.username, s.password); err != nil {
		return errors.Join(fmt.Errorf("auth: %w", err), wire.Close())
	}

	s.conn = conn
	s.wire = wire
	s.connected = true
	return nil
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
		if since != 0 || until != 0 {
			errChan <- errors.New("POP3 date filters are unsupported; omit --since and --until")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()

		if err := s.connectLocked(ctx); err != nil {
			errChan <- err
			return
		}
		finish := s.wire.beginIO(ctx)
		defer finish()

		// POP3 only supports INBOX
		if err := s.fetchFolder(ctx, "INBOX", msgChan, since, until); err != nil {
			s.connected = false
			_ = s.wire.Close()
			errChan <- fmt.Errorf("fetch folder: %w", err)
			return
		}
	}()

	return msgChan, errChan
}

func (s *POP3Source) fetchFolder(ctx context.Context, folder string, msgChan chan<- *plugins.Message, since, until int64) error {
	// Get message count
	count, _, err := s.conn.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}

	if count == 0 {
		return nil
	}

	// Fetch each message
	for i := 1; i <= count; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// RETR retrieves the full message
		buf, err := s.conn.RetrRaw(i)
		if err != nil {
			return fmt.Errorf("retr message %d: %w", i, err)
		}

		raw := buf.Bytes()

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

		pluginsMsg := &plugins.Message{
			UID:          strconv.Itoa(i),
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
