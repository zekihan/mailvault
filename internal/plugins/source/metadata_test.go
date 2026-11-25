package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
)

const reconnectRaw = "Subject: synthetic fixture\r\n\r\nbody\r\n"

type reconnectRefreshServer struct {
	t           *testing.T
	attempt     atomic.Int32
	connections atomic.Int32
}

func (server *reconnectRefreshServer) serve(conn net.Conn, _ <-chan struct{}) {
	attempt := server.connections.Add(1)
	server.attempt.Store(attempt)
	_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		if !server.handleLine(conn, attempt, scanner.Text()) {
			return
		}
	}
}

func (server *reconnectRefreshServer) handleLine(conn net.Conn, attempt int32, line string) bool {
	parts := strings.Fields(line)
	tag := parts[0]
	switch parts[1] {
	case "LOGIN":
		_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
	case "EXAMINE":
		_, _ = fmt.Fprintf(conn, "* 3 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n%s OK selected\r\n", tag)
	case "UID":
		return server.handleUID(conn, attempt, parts, line, tag)
	default:
		_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
	}
	return true
}

func (server *reconnectRefreshServer) handleUID(conn net.Conn, attempt int32, parts []string, line, tag string) bool {
	switch parts[2] {
	case "SEARCH":
		server.handleSearch(conn, attempt, line, tag)
	case "FETCH":
		return server.handleFetch(conn, attempt, parts, line, tag)
	}
	return true
}

func (server *reconnectRefreshServer) handleSearch(conn net.Conn, attempt int32, line, tag string) {
	search := "2 3"
	if attempt == 2 {
		if !strings.HasSuffix(line, "NOT (UID 1:2)") {
			server.t.Errorf("retry search failed to exclude delivered and streamed UIDs: %s", line)
		}
		search = "3"
	}
	_, _ = fmt.Fprintf(conn, "* SEARCH %s\r\n%s OK searched\r\n", search, tag)
}

func (server *reconnectRefreshServer) handleFetch(conn net.Conn, attempt int32, parts []string, line, tag string) bool {
	if !strings.Contains(line, "BODY[") {
		server.handleFlagsFetch(conn, parts, tag)
		return true
	}
	if attempt == 1 {
		_, _ = fmt.Fprintf(conn, "* 2 FETCH (UID 2 BODY[] {%d}\r\n%s)\r\n", len(reconnectRaw), reconnectRaw)
		return false
	}
	if parts[3] != "3" {
		server.t.Errorf("retry body fetch = %q, want only UID 3", parts[3])
	}
	_, _ = fmt.Fprintf(conn, "* 3 FETCH (UID 3 BODY[] {%d}\r\n%s)\r\n%s OK fetched\r\n", len(reconnectRaw), reconnectRaw, tag)
	return true
}

func (server *reconnectRefreshServer) handleFlagsFetch(conn net.Conn, parts []string, tag string) {
	ids, err := parseUIDSet(parts[3])
	if err != nil {
		server.t.Error(err)
		return
	}
	for _, uid := range ids {
		_, _ = fmt.Fprintf(conn, "* %d FETCH (UID %d FLAGS (\\Seen))\r\n", uid, uid)
	}
	_, _ = fmt.Fprintf(conn, "%s OK flags sent\r\n", tag)
}

func TestIMAPReconnectRefreshesOnlyDeliveredUIDs(t *testing.T) {
	server := &reconnectRefreshServer{t: t}
	host, port := protocolServer(t, server.serve)
	src := &IMAPSource{host: host, port: port, username: "fixture", password: "fixture", connTimeout: time.Second, readTimeout: time.Second, maxConns: 1}
	defer src.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = withReconnectKnownUIDs(ctx)
	var refreshed []string
	ctx = plugins.WithMetadataObserver(ctx, func(msg *plugins.Message) error {
		if msg.UID != "1" {
			return fmt.Errorf("UID %s has no delivery receipt", msg.UID)
		}
		refreshed = append(refreshed, msg.UID)
		return nil
	})
	messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	var fetched []string
	for msg := range messages {
		fetched = append(fetched, msg.UID)
	}
	for err := range failures {
		t.Fatal(err)
	}
	if !slices.Equal(fetched, []string{"2", "3"}) || !slices.Equal(refreshed, []string{"1", "1"}) || server.connections.Load() != 2 {
		t.Fatalf("fetched %v, refreshed %v, connections %d", fetched, refreshed, server.connections.Load())
	}
}

func withReconnectKnownUIDs(ctx context.Context) context.Context {
	return plugins.WithKnownUIDs(ctx, func(string, string) (map[string]bool, error) {
		return map[string]bool{"1": true}, nil
	})
}

type metadataRefreshServer struct {
	commands chan string
}

func (server *metadataRefreshServer) serve(conn net.Conn, _ <-chan struct{}) {
	_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return
		}
		server.handleCommand(conn, parts, line)
	}
}

func (server *metadataRefreshServer) handleCommand(conn net.Conn, parts []string, line string) {
	tag := parts[0]
	switch parts[1] {
	case "LOGIN":
		_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
	case "EXAMINE":
		_, _ = fmt.Fprintf(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n%s OK selected\r\n", tag)
	case "UID":
		server.commands <- line
		if parts[2] == "FETCH" {
			_, _ = fmt.Fprintf(conn, "* 1 FETCH (UID 42 FLAGS (\\Seen custom))\r\n%s OK fetched\r\n", tag)
		} else {
			_, _ = fmt.Fprintf(conn, "* SEARCH\r\n%s OK searched\r\n", tag)
		}
	default:
		_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
	}
}

func TestMetadataRefreshDoesNotFetchBody(t *testing.T) {
	server := &metadataRefreshServer{commands: make(chan string, 10)}
	host, port := protocolServer(t, server.serve)
	src := &IMAPSource{host: host, port: port, username: "test", password: "test", connTimeout: time.Second, readTimeout: time.Second, maxConns: 1}
	defer src.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ctx = plugins.WithKnownUIDs(ctx, func(string, string) (map[string]bool, error) { return map[string]bool{"42": true}, nil })
	observed := make(chan *plugins.Message, 1)
	ctx = plugins.WithMetadataObserver(ctx, func(msg *plugins.Message) error { observed <- msg; return nil })
	messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	for range messages {
		t.Error("unexpected body")
	}
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case msg := <-observed:
		if msg.UID != "42" || len(msg.Flags) != 2 || msg.Flags[1] != "custom" {
			t.Fatalf("metadata: %+v", msg)
		}
	default:
		t.Fatal("no metadata")
	}
	for len(server.commands) > 0 {
		command := <-server.commands
		if strings.Contains(command, "FETCH") && strings.Contains(command, "BODY") {
			t.Fatalf("body fetched: %s", command)
		}
	}
}

type blockedFlagsFetchServer struct {
	fetchStarted chan struct{}
}

func (server *blockedFlagsFetchServer) serve(conn net.Conn, stop <-chan struct{}) {
	_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) < 2 {
			return
		}
		if !server.handleCommand(conn, stop, parts) {
			return
		}
	}
}

func (server *blockedFlagsFetchServer) handleCommand(conn net.Conn, stop <-chan struct{}, parts []string) bool {
	tag := parts[0]
	switch parts[1] {
	case "LOGIN":
		_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
	case "EXAMINE":
		_, _ = fmt.Fprintf(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n%s OK selected\r\n", tag)
	case "UID":
		return server.handleUID(conn, stop, parts, tag)
	default:
		_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
	}
	return true
}

func (server *blockedFlagsFetchServer) handleUID(conn net.Conn, stop <-chan struct{}, parts []string, tag string) bool {
	if len(parts) > 2 && parts[2] == "FETCH" {
		server.fetchStarted <- struct{}{}
		<-stop
		return false
	}
	_, _ = fmt.Fprintf(conn, "* SEARCH\r\n%s OK searched\r\n", tag)
	return true
}

func TestMetadataRefreshCancellationInterruptsBlockedFlagsFetch(t *testing.T) {
	server := &blockedFlagsFetchServer{fetchStarted: make(chan struct{}, 1)}
	host, port := protocolServer(t, server.serve)
	src := &IMAPSource{host: host, port: port, username: "fixture", password: "fixture", connTimeout: time.Second, readTimeout: 10 * time.Second, maxConns: 1}
	defer src.Close()
	ctx, cancel := context.WithCancel(t.Context())
	ctx = plugins.WithKnownUIDs(ctx, func(string, string) (map[string]bool, error) { return map[string]bool{"42": true}, nil })
	ctx = plugins.WithMetadataObserver(ctx, func(*plugins.Message) error { return nil })
	messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	select {
	case <-server.fetchStarted:
		cancel()
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("metadata flags fetch did not start")
	}
	deadline := time.After(2 * time.Second)
	for messages != nil || failures != nil {
		select {
		case _, ok := <-messages:
			if !ok {
				messages = nil
			}
		case err, ok := <-failures:
			if !ok {
				failures = nil
			} else if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("fetch error = %v, want cancellation", err)
			}
		case <-deadline:
			t.Fatal("metadata flags fetch did not stop after cancellation")
		}
	}
}
