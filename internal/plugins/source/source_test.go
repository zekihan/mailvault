package source

import (
	"bufio"
	"context"
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/testutil/mailtest"
)

func protocolServer(t *testing.T, serve func(net.Conn, <-chan struct{})) (string, int) {
	t.Helper()
	return mailtest.ProtocolServer(t, serve)
}

const imapFetchRaw = "Message-ID: <fixture@example.invalid>\r\nSubject: test\r\n\r\nbody\r\n"

type imapFetchUIDsServer struct {
	commands chan string
}

func (server *imapFetchUIDsServer) serve(conn net.Conn, _ <-chan struct{}) {
	_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		server.commands <- line
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return
		}
		server.handleCommand(conn, parts)
	}
}

func (server *imapFetchUIDsServer) handleCommand(conn net.Conn, parts []string) {
	tag := parts[0]
	switch parts[1] {
	case "LOGIN":
		_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
	case "CAPABILITY":
		_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", tag)
	case "EXAMINE":
		_, _ = fmt.Fprintf(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n%s OK [READ-ONLY] selected\r\n", tag)
	case "LIST":
		_, _ = fmt.Fprintf(conn, "* LIST () \"/\" \"INBOX\"\r\n%s OK listed\r\n", tag)
	case "UID":
		server.handleUID(conn, parts, tag)
	case "SEARCH":
		_, _ = fmt.Fprintf(conn, "* SEARCH 1\r\n%s OK searched\r\n", tag)
	default:
		_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
	}
}

func (server *imapFetchUIDsServer) handleUID(conn net.Conn, parts []string, tag string) {
	if parts[2] == "SEARCH" {
		_, _ = fmt.Fprintf(conn, "* SEARCH 42\r\n%s OK searched\r\n", tag)
		return
	}
	_, _ = fmt.Fprintf(conn, "* 1 FETCH (UID 42 INTERNALDATE \"01-Jan-2024 00:00:00 +0000\" BODY[] {%d}\r\n%s)\r\n%s OK fetched\r\n", len(imapFetchRaw), imapFetchRaw, tag)
}

func drainIMAPFetch(ctx context.Context, t *testing.T, src *IMAPSource) []*plugins.Message {
	t.Helper()
	messages, errs := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	var got []*plugins.Message
	for messages != nil || errs != nil {
		select {
		case msg, ok := <-messages:
			if !ok {
				messages = nil
			} else {
				got = append(got, msg)
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
			} else if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	return got
}

func checkIMAPFetchUIDCommand(t *testing.T, commands <-chan string) {
	t.Helper()
	var fetch string
	for len(commands) > 0 {
		command := <-commands
		if strings.Contains(command, "UID FETCH") {
			fetch = command
		}
	}
	if !strings.Contains(fetch, "42") || !strings.Contains(fetch, "INTERNALDATE") {
		t.Fatalf("expected UID FETCH with internal date, got %q", fetch)
	}
}

func TestIMAPFetchUsesUIDsAndInternalDate(t *testing.T) {
	server := &imapFetchUIDsServer{commands: make(chan string, 20)}
	host, port := protocolServer(t, server.serve)
	src := &IMAPSource{host: host, port: port, username: "test-user", password: "test-value", connTimeout: time.Second, readTimeout: time.Second, maxConns: 5}
	defer src.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := drainIMAPFetch(ctx, t, src)
	if len(got) != 1 || got[0].UID != "42" || string(got[0].Raw) != imapFetchRaw || got[0].InternalDate != "2024-01-01T00:00:00Z" {
		t.Fatalf("unexpected messages: %+v", got)
	}
	checkIMAPFetchUIDCommand(t, server.commands)
	folders, err := src.ListFolders(ctx)
	if err != nil || len(folders) != 1 || folders[0] != "INBOX" {
		t.Fatalf("ListFolders() = %v, %v", folders, err)
	}
}

const truncatedRecoveredRaw = "Message-ID: <recovered@example.invalid>\r\nSubject: recovered\r\n\r\ncomplete body\r\n"

type truncatedReconnectServer struct {
	mu                   sync.Mutex
	connections, fetches int
}

func (server *truncatedReconnectServer) serve(conn net.Conn, _ <-chan struct{}) {
	server.mu.Lock()
	server.connections++
	connection := server.connections
	server.mu.Unlock()
	_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) < 2 {
			return
		}
		if !server.handleCommand(conn, connection, parts) {
			return
		}
	}
}

func (server *truncatedReconnectServer) handleCommand(conn net.Conn, connection int, parts []string) bool {
	tag := parts[0]
	switch strings.ToUpper(parts[1]) {
	case "LOGIN":
		_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
	case "CAPABILITY":
		_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", tag)
	case "EXAMINE":
		_, _ = fmt.Fprintf(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n%s OK [READ-ONLY] selected\r\n", tag)
	case "UID":
		return server.handleUID(conn, connection, parts, tag)
	default:
		_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
	}
	return true
}

func (server *truncatedReconnectServer) handleUID(conn net.Conn, connection int, parts []string, tag string) bool {
	if len(parts) < 3 {
		return false
	}
	switch strings.ToUpper(parts[2]) {
	case "SEARCH":
		_, _ = fmt.Fprintf(conn, "* SEARCH 42\r\n%s OK searched\r\n", tag)
	case "FETCH":
		return server.handleFetch(conn, connection, tag)
	}
	return true
}

func (server *truncatedReconnectServer) handleFetch(conn net.Conn, connection int, tag string) bool {
	server.mu.Lock()
	server.fetches++
	server.mu.Unlock()
	if connection == 1 {
		_, _ = fmt.Fprintf(conn, "* 1 FETCH (UID 42 BODY[] {%d}\r\n%s", len(truncatedRecoveredRaw), truncatedRecoveredRaw[:len(truncatedRecoveredRaw)/2])
		return false
	}
	_, _ = fmt.Fprintf(conn, "* 1 FETCH (UID 42 INTERNALDATE \"01-Jan-2024 00:00:00 +0000\" BODY[] {%d}\r\n%s)\r\n%s OK fetched\r\n", len(truncatedRecoveredRaw), truncatedRecoveredRaw, tag)
	return true
}

func drainTruncatedReconnect(ctx context.Context, t *testing.T, src *IMAPSource) []*plugins.Message {
	t.Helper()
	messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	var got []*plugins.Message
	for messages != nil || failures != nil {
		select {
		case msg, ok := <-messages:
			if !ok {
				messages = nil
			} else {
				got = append(got, msg)
			}
		case err, ok := <-failures:
			if !ok {
				failures = nil
			} else if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	return got
}

func TestIMAPFetchReconnectsAfterTruncatedMessage(t *testing.T) {
	server := &truncatedReconnectServer{}
	host, port := protocolServer(t, server.serve)
	src := &IMAPSource{host: host, port: port, username: "test-user", password: "test-password", connTimeout: time.Second, readTimeout: time.Second, maxConns: 1}
	defer src.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got := drainTruncatedReconnect(ctx, t, src)
	server.mu.Lock()
	gotConnections, gotFetches := server.connections, server.fetches
	server.mu.Unlock()
	if gotConnections != 2 || gotFetches != 2 {
		if len(got) > 0 {
			t.Fatalf("connections=%d fetches=%d; want reconnect and retry, got UID=%q raw=%q", gotConnections, gotFetches, got[0].UID, got[0].Raw)
		}
		t.Fatalf("connections=%d fetches=%d; want reconnect and retry, got messages %+v", gotConnections, gotFetches, got)
	}
	if len(got) != 1 || string(got[0].Raw) != truncatedRecoveredRaw || got[0].UID != "42" {
		t.Fatalf("recovered messages = %+v", got)
	}
}

func TestSourceDurationConfig(t *testing.T) {
	values := map[string]any{
		"host": "example.invalid", "username": "user", "password_ref": "synthetic-value",
		"connection_timeout": time.Second, "read_timeout": 60 * time.Millisecond,
	}
	imapSource, err := NewIMapSource(values)
	if err != nil {
		t.Fatal(err)
	}
	popSource, err := NewPOP3Source(values)
	if err != nil {
		t.Fatal(err)
	}
	if mustAssert[*IMAPSource](t, imapSource).readTimeout != 60*time.Millisecond || mustAssert[*POP3Source](t, popSource).readTimeout != 60*time.Millisecond {
		t.Fatal("typed configuration durations were discarded")
	}
}

type stallServerFixture struct {
	kind, phase string
	reached     chan struct{}
}

func (fixture *stallServerFixture) stall(stop <-chan struct{}) {
	select {
	case fixture.reached <- struct{}{}:
	default:
	}
	<-stop
}

func (fixture *stallServerFixture) serve(conn net.Conn, stop <-chan struct{}) {
	if fixture.phase == "greeting" {
		fixture.stall(stop)
		return
	}
	if fixture.kind == "imap" {
		_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
	} else {
		_, _ = fmt.Fprint(conn, "+OK ready\r\n")
	}
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) == 0 {
			return
		}
		if fixture.handleLine(conn, stop, parts) {
			return
		}
	}
}

func (fixture *stallServerFixture) handleLine(conn net.Conn, stop <-chan struct{}, parts []string) bool {
	if fixture.kind == "pop3" {
		return fixture.handlePOP3Line(conn, stop, parts)
	}
	return fixture.handleIMAPLine(conn, stop, parts)
}

func (fixture *stallServerFixture) handlePOP3Line(conn net.Conn, stop <-chan struct{}, parts []string) bool {
	if (fixture.phase == "auth" && parts[0] == "USER") || (fixture.phase == "fetch" && parts[0] == "STAT") {
		fixture.stall(stop)
		return true
	}
	_, _ = fmt.Fprint(conn, "+OK\r\n")
	return false
}

func (fixture *stallServerFixture) handleIMAPLine(conn net.Conn, stop <-chan struct{}, parts []string) bool {
	if len(parts) < 2 {
		return true
	}
	if (fixture.phase == "auth" && parts[1] == "LOGIN") || (fixture.phase == "fetch" && parts[1] == "EXAMINE") {
		fixture.stall(stop)
		return true
	}
	if parts[1] == "CAPABILITY" {
		_, _ = fmt.Fprint(conn, "* CAPABILITY IMAP4rev1\r\n")
	}
	_, _ = fmt.Fprintf(conn, "%s OK done\r\n", parts[0])
	return false
}

type readTimeoutCase struct {
	kind, phase string
	cancel      bool
}

func (fixture *readTimeoutCase) readTimeout() time.Duration {
	if fixture.cancel {
		return 5 * time.Second
	}
	return 60 * time.Millisecond
}

func (fixture *readTimeoutCase) openSource(t *testing.T, host string, port int) plugins.Source {
	t.Helper()
	if fixture.kind == "imap" {
		return &IMAPSource{host: host, port: port, username: "test-user", password: "test-value", connTimeout: time.Second, readTimeout: fixture.readTimeout(), maxConns: 1}
	}
	return &POP3Source{host: host, port: port, username: "test-user", password: "test-value", connTimeout: time.Second, readTimeout: fixture.readTimeout()}
}

func runStalledOperation(ctx context.Context, src plugins.Source, phase string) error {
	if phase != "fetch" {
		return src.Connect(ctx)
	}
	messages, errs := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	var failure error
	for messages != nil || errs != nil {
		select {
		case _, ok := <-messages:
			if !ok {
				messages = nil
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
			} else if err != nil {
				failure = err
			}
		}
	}
	return failure
}

func (fixture *readTimeoutCase) run(t *testing.T) {
	t.Helper()
	server := &stallServerFixture{kind: fixture.kind, phase: fixture.phase, reached: make(chan struct{}, 1)}
	host, port := protocolServer(t, server.serve)
	src := fixture.openSource(t, host, port)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- runStalledOperation(ctx, src, fixture.phase) }()
	select {
	case <-server.reached:
	case <-time.After(time.Second):
		t.Fatal("operation did not reach stalled server")
	}
	if fixture.cancel {
		cancel()
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled operation succeeded")
		}
		_ = src.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("stalled operation ignored configured timeout/cancellation")
	}
}

func TestSourceReadTimeoutAndCancellation(t *testing.T) {
	for _, kind := range []string{"imap", "pop3"} {
		for _, phase := range []string{"greeting", "auth", "fetch"} {
			for _, cancelOperation := range []bool{false, true} {
				t.Run(kind+"/"+phase+"/cancel="+strconv.FormatBool(cancelOperation), func(t *testing.T) {
					(&readTimeoutCase{kind: kind, phase: phase, cancel: cancelOperation}).run(t)
				})
			}
		}
	}
}

func TestSourcesRejectFractionalIntegerSettings(t *testing.T) {
	base := map[string]any{
		"host": "example.invalid", "username": "user", "password_ref": "synthetic-value",
	}
	for _, key := range []string{"port", "max_connections"} {
		values := map[string]any{key: 993.5}
		maps.Copy(values, base)
		if _, err := NewIMapSource(values); err == nil {
			t.Errorf("IMAP accepted fractional %s", key)
		}
	}
	values := map[string]any{"port": 995.5}
	maps.Copy(values, base)
	if _, err := NewPOP3Source(values); err == nil {
		t.Error("POP3 accepted fractional port")
	}
}
