package source

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/config"
	"sync"
)

// IMAPSource implements the Source interface for IMAP.
type IMAPSource struct {
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
	maxConns    int
	client      *imapclient.Client
	wire        *deadlineConn
	connected   bool
	mu          sync.Mutex
}

func NewIMapSource(configMap map[string]interface{}) (plugins.Source, error) {
	host, _ := config.ParseString(configMap, config.SourceConfigHost, "")
	port, _ := config.ParseInt(configMap, config.SourceConfigPort, 993)
	username, _ := config.ParseString(configMap, config.SourceConfigUsername, "")
	passwordRef, _ := config.ParseString(configMap, config.SourceConfigPasswordRef, "")
	useTLS, _ := config.ParseBool(configMap, config.SourceConfigUseTLS, true)
	startTLS, _ := config.ParseBool(configMap, config.SourceConfigStartTLS, false)
	folders, _ := config.ParseStringSlice(configMap, config.SourceConfigFolders)
	connTimeout, _ := config.ParseDuration(configMap, config.SourceConfigConnectionTimeout, 30*time.Second)
	readTimeout, _ := config.ParseDuration(configMap, config.SourceConfigReadTimeout, 60*time.Second)
	maxConns, _ := config.ParseInt(configMap, config.SourceConfigMaxConnections, 5)

	if host == "" {
		return nil, errors.New("host is required")
	}
	if username == "" {
		return nil, errors.New("username is required")
	}
	if passwordRef == "" {
		return nil, errors.New("password_ref is required")
	}
	if maxConns <= 0 {
		maxConns = 5
	}

	// Default folders
	if len(folders) == 0 {
		folders = []string{"INBOX"}
	}

	return &IMAPSource{
		name:        "imap:" + host, // Will be overridden by registry
		host:        host,
		port:        port,
		username:    username,
		password:    passwordRef, // Already resolved by config loader
		useTLS:      useTLS,
		startTLS:    startTLS,
		folders:     folders,
		connTimeout: connTimeout,
		readTimeout: readTimeout,
		maxConns:    maxConns,
	}, nil
}

// SetName sets the source name (called by registry).
func (s *IMAPSource) SetName(name string) {
	s.name = name
}

func (s *IMAPSource) Name() string {
	return s.name
}

func (s *IMAPSource) Type() string {
	return "imap"
}

func (s *IMAPSource) Connect(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connectLocked(ctx)
}

func (s *IMAPSource) connectLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.connected {
		select {
		case <-s.client.Closed():
			s.connected = false
		default:
			return nil
		}
	}

	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	dialer := &net.Dialer{Timeout: s.connTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	wire := &deadlineConn{Conn: raw, timeout: s.readTimeout}
	finish := wire.beginIO(ctx)
	defer finish()
	tlsConfig := &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}
	opts := &imapclient.Options{TLSConfig: tlsConfig}
	var client *imapclient.Client
	if s.useTLS {
		secure := tls.Client(wire, tlsConfig)
		err = secure.HandshakeContext(ctx)
		if err == nil {
			client = imapclient.New(secure, opts)
		}
	} else if s.startTLS {
		client, err = imapclient.NewStartTLS(wire, opts)
	} else {
		client = imapclient.New(wire, opts)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("establish TLS: %w", err), raw.Close())
	}
	if err := client.WaitGreeting(); err != nil {
		return errors.Join(fmt.Errorf("greeting: %w", err), client.Close())
	}
	if err := client.Login(s.username, s.password).Wait(); err != nil {
		return errors.Join(fmt.Errorf("login: %w", err), client.Close())
	}

	s.client = client
	s.wire = wire
	s.connected = true
	return nil
}

func (s *IMAPSource) ListFolders(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.connectLocked(ctx); err != nil {
		return nil, err
	}
	finish := s.wire.beginIO(ctx)
	defer finish()

	list := s.client.List("", "*", &imap.ListOptions{})
	data, err := list.Collect()
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}

	var folders []string
	for _, mb := range data {
		folders = append(folders, mb.Mailbox)
	}

	return folders, nil
}

func (s *IMAPSource) FetchMessages(ctx context.Context, folders []string, since, until int64) (<-chan *plugins.Message, <-chan error) {
	msgChan := make(chan *plugins.Message, 100)
	errChan := make(chan error, 1)

	go func() {
		defer close(msgChan)
		defer close(errChan)
		s.mu.Lock()
		defer s.mu.Unlock()

		if err := s.connectLocked(ctx); err != nil {
			errChan <- err
			return
		}
		finish := s.wire.beginIO(ctx)
		defer finish()

		for _, folder := range folders {
			select {
			case <-ctx.Done():
				errChan <- ctx.Err()
				return
			default:
			}

			if err := s.fetchFolder(ctx, folder, msgChan, since, until); err != nil {
				errChan <- fmt.Errorf("fetch folder %q: %w", folder, err)
				return
			}
		}
	}()

	return msgChan, errChan
}

func (s *IMAPSource) fetchFolder(ctx context.Context, folder string, msgChan chan<- *plugins.Message, since, until int64) error {
	progress := plugins.FetchProgress{Folder: folder, Total: -1, Stage: "searching"}
	plugins.ReportFetchProgress(ctx, progress)

	// Select mailbox
	selectCmd := s.client.Select(folder, &imap.SelectOptions{ReadOnly: true})
	if _, err := selectCmd.Wait(); err != nil {
		return fmt.Errorf("select %q: %w", folder, err)
	}

	// Build search criteria
	criteria := timestampSearchCriteria(since, until)

	// Search for messages
	search := s.client.UIDSearch(&criteria, nil)
	searchData, err := search.Wait()
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}

	uids := searchData.AllUIDs()
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

	// Fetch messages in batches
	batchSize := s.maxConns
	for i := 0; i < len(uids); i += batchSize {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		end := i + batchSize
		if end > len(uids) {
			end = len(uids)
		}

		batch := uids[i:end]
		if err := s.fetchBatch(ctx, folder, batch, msgChan, since, until, func() {
			progress.Fetched++
			plugins.ReportFetchProgress(ctx, progress)
		}); err != nil {
			return err
		}
	}

	completed()
	return nil
}

func (s *IMAPSource) fetchBatch(ctx context.Context, folder string, uids []imap.UID, msgChan chan<- *plugins.Message, since, until int64, fetched func()) (err error) {
	// Fetch RFC822 for each UID
	var seqSet imap.UIDSet
	for _, uid := range uids {
		seqSet.AddNum(uid)
	}

	fetch := s.client.Fetch(seqSet, &imap.FetchOptions{
		BodySection:  []*imap.FetchItemBodySection{{}},
		Envelope:     true,
		InternalDate: true,
	})

	defer func() {
		err = errors.Join(err, fetch.Close())
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

		// Collect message data into buffer
		buf, err := msg.Collect()
		if err != nil {
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

		// Get RFC822 body
		section := &imap.FetchItemBodySection{}
		raw := buf.FindBodySection(section)
		if raw == nil {
			continue
		}

		// Parse headers for Message-ID
		messageID := extractMessageIDFromEnvelope(buf.Envelope)
		if messageID == "" {
			// Fallback: parse from raw
			messageID = dedup.ExtractMessageID(raw)
		}

		// Compute content hash
		contentHash := dedup.ComputeContentHash(raw)

		// Prefer the server's arrival date to the sender's Date header.
		internalDate := ""
		if !buf.InternalDate.IsZero() {
			internalDate = buf.InternalDate.Format(time.RFC3339)
		} else if buf.Envelope != nil && !buf.Envelope.Date.IsZero() {
			internalDate = buf.Envelope.Date.Format(time.RFC3339)
		}

		pluginsMsg := &plugins.Message{
			UID:          strconv.FormatUint(uint64(buf.UID), 10),
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

func (s *IMAPSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil {
		err := s.client.Close()
		s.client = nil
		s.connected = false
		return err
	}
	return nil
}

// extractMessageIDFromEnvelope extracts Message-ID from IMAP envelope.
func extractMessageIDFromEnvelope(env *imap.Envelope) string {
	if env == nil || env.MessageID == "" {
		return ""
	}
	// Envelope.MessageID already has < > stripped by go-imap
	return env.MessageID
}
