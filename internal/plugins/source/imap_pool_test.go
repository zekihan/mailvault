package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
)

type imapPoolServer struct {
	count             int
	blockFetch        bool
	rejectSecondFetch bool
	accepted          chan struct{}
	fetchStarted      chan struct{}
	mu                sync.Mutex
	active, maxActive int
	connectionCount   int
	got               map[string]int
}

func (server *imapPoolServer) serve(conn net.Conn, stop <-chan struct{}) {
	server.mu.Lock()
	server.connectionCount++
	connectionID := server.connectionCount
	server.mu.Unlock()
	server.accepted <- struct{}{}
	_, _ = fmt.Fprint(conn, "* OK fixture ready\r\n")
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) < 2 {
			return
		}
		if !server.handleCommand(conn, stop, connectionID, parts) {
			return
		}
	}
}

func (server *imapPoolServer) handleCommand(conn net.Conn, stop <-chan struct{}, connectionID int, parts []string) bool {
	tag := parts[0]
	switch strings.ToUpper(parts[1]) {
	case "LOGIN":
		_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
	case "CAPABILITY":
		_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", tag)
	case "EXAMINE":
		_, _ = fmt.Fprintf(conn, "* %d EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n* OK [UIDNEXT %d] next\r\n%s OK [READ-ONLY] selected\r\n", server.count, server.count+1, tag)
	case "UID":
		return server.handleUID(conn, stop, connectionID, parts, tag)
	default:
		_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
	}
	return true
}

func (server *imapPoolServer) handleUID(conn net.Conn, stop <-chan struct{}, connectionID int, parts []string, tag string) bool {
	if len(parts) < 4 {
		_, _ = fmt.Fprintf(conn, "%s BAD incomplete UID command\r\n", tag)
		return true
	}
	switch strings.ToUpper(parts[2]) {
	case "SEARCH":
		server.handlePoolSearch(conn, tag)
	case "FETCH":
		return server.handlePoolFetch(conn, stop, connectionID, parts, tag)
	}
	return true
}

func (server *imapPoolServer) handlePoolSearch(conn net.Conn, tag string) {
	_, _ = fmt.Fprint(conn, "* SEARCH")
	for i := 1; i <= server.count; i++ {
		_, _ = fmt.Fprintf(conn, " %d", i)
	}
	_, _ = fmt.Fprintf(conn, "\r\n%s OK searched\r\n", tag)
}

func (server *imapPoolServer) handlePoolFetch(conn net.Conn, stop <-chan struct{}, connectionID int, parts []string, tag string) bool {
	server.mu.Lock()
	server.active++
	if server.active > server.maxActive {
		server.maxActive = server.active
	}
	server.mu.Unlock()
	select {
	case server.fetchStarted <- struct{}{}:
	default:
	}
	if server.rejectSecondFetch {
		return server.handlePoolRejectedFetch(conn, stop, connectionID, tag)
	}
	if server.blockFetch {
		<-stop
	} else {
		<-time.After(25 * time.Millisecond)
	}
	return server.writePoolFetch(conn, parts, tag)
}

func (server *imapPoolServer) handlePoolRejectedFetch(conn net.Conn, stop <-chan struct{}, connectionID int, tag string) bool {
	if connectionID == 1 {
		<-stop
		return false
	}
	_, _ = fmt.Fprintf(conn, "%s NO server policy rejection\r\n", tag)
	return false
}

func (server *imapPoolServer) writePoolFetch(conn net.Conn, parts []string, tag string) bool {
	ids, err := parseUIDSet(parts[3])
	if err != nil {
		server.mu.Lock()
		server.active--
		server.mu.Unlock()
		_, _ = fmt.Fprintf(conn, "%s BAD invalid uid set\r\n", tag)
		return true
	}
	for _, id := range ids {
		raw := fmt.Sprintf("Message-ID: <pool-%d@example.invalid>\r\nSubject: %d\r\n\r\nbody-%d\r\n", id, id, id)
		_, _ = fmt.Fprintf(conn, "* %d FETCH (UID %d INTERNALDATE \"01-Jan-2024 00:00:00 +0000\" FLAGS (\\Seen \\Flagged) BODY[] {%d}\r\n%s)\r\n", id, id, len(raw), raw)
		server.mu.Lock()
		server.got[strconv.Itoa(id)]++
		server.mu.Unlock()
	}
	_, _ = fmt.Fprintf(conn, "%s OK fetched\r\n", tag)
	server.mu.Lock()
	server.active--
	server.mu.Unlock()
	return true
}

type imapPoolFixture struct {
	host         string
	port         int
	accepted     <-chan struct{}
	fetchStarted <-chan struct{}
	maxActive    func() int
	got          func(map[string]int)
}

func newIMAPPoolFixture(t *testing.T, count int, blockFetch bool, rejectSecondFetch ...bool) *imapPoolFixture {
	t.Helper()
	server := &imapPoolServer{count: count, blockFetch: blockFetch, accepted: make(chan struct{}, 8), fetchStarted: make(chan struct{}, 8), got: make(map[string]int)}
	if len(rejectSecondFetch) > 0 && rejectSecondFetch[0] {
		server.rejectSecondFetch = true
	}
	host, port := protocolServer(t, server.serve)
	maxFn := func() int { server.mu.Lock(); defer server.mu.Unlock(); return server.maxActive }
	gotFn := func(dst map[string]int) {
		server.mu.Lock()
		defer server.mu.Unlock()
		maps.Copy(dst, server.got)
	}
	return &imapPoolFixture{host: host, port: port, accepted: server.accepted, fetchStarted: server.fetchStarted, maxActive: maxFn, got: gotFn}
}

func parseUIDSet(value string) ([]int, error) {
	value = strings.Trim(value, "(")
	value = strings.TrimSuffix(value, ")")
	var ids []int
	for part := range strings.SplitSeq(value, ",") {
		bounds := strings.Split(part, ":")
		first, err := strconv.Atoi(bounds[0])
		if err != nil {
			return nil, err
		}
		last := first
		if len(bounds) == 2 {
			last, err = strconv.Atoi(bounds[1])
			if err != nil {
				return nil, err
			}
		}
		for id := first; id <= last; id++ {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func TestIMAPConnectionPoolBoundsFetchesAndPreservesReceipts(t *testing.T) {
	const count = 50
	pool := newIMAPPoolFixture(t, count, false)
	src := &IMAPSource{host: pool.host, port: pool.port, username: "fixture-user", password: "fixture-password", useTLS: false, maxConns: 2, connTimeout: time.Second, readTimeout: time.Second}
	defer src.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	gotMessages := drainPoolMessages(ctx, t, messages, failures)
	checkPoolBounds(t, gotMessages, pool.maxActive(), pool.accepted, pool.got, count)
}

func drainPoolMessages(ctx context.Context, t *testing.T, messages <-chan *plugins.Message, failures <-chan error) int {
	t.Helper()
	gotMessages := 0
	for messages != nil || failures != nil {
		select {
		case msg, ok := <-messages:
			if !ok {
				messages = nil
			} else {
				gotMessages++
				if strings.Join(msg.Flags, ",") != "\\Seen,\\Flagged" {
					t.Errorf("message flags = %v", msg.Flags)
				}
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
	return gotMessages
}

func checkPoolBounds(t *testing.T, gotMessages, maxConcurrent int, accepted <-chan struct{}, fetches func(map[string]int), count int) {
	t.Helper()
	if gotMessages != count || maxConcurrent != 2 {
		t.Fatalf("messages=%d max concurrent FETCH=%d, want %d and 2", gotMessages, maxConcurrent, count)
	}
	if n := len(accepted); n != 2 {
		t.Fatalf("opened %d IMAP sessions, want 2", n)
	}
	seen := make(map[string]int)
	fetches(seen)
	checkPoolUIDCoverage(t, seen, count)
}

func checkPoolUIDCoverage(t *testing.T, seen map[string]int, count int) {
	t.Helper()
	if len(seen) != count {
		t.Fatalf("fetched %d distinct UIDs, want %d", len(seen), count)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("UID %s fetched %d times", id, n)
		}
	}
}

func TestIMAPConnectionPoolCancellationInterruptsBlockedFetch(t *testing.T) {
	pool := newIMAPPoolFixture(t, 50, true)
	src := &IMAPSource{host: pool.host, port: pool.port, username: "fixture-user", password: "fixture-password", useTLS: false, maxConns: 2, connTimeout: time.Second, readTimeout: 5 * time.Second}
	ctx, cancel := context.WithCancel(t.Context())
	messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	select {
	case <-pool.fetchStarted:
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("no pooled FETCH started")
	}
	deadline := time.After(3 * time.Second)
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
			t.Fatal("fetch did not stop after cancellation")
		}
	}
	_ = src.Close()
}

func TestIMAPConnectionPoolDoesNotRetryTaggedFetchRejection(t *testing.T) {
	pool := newIMAPPoolFixture(t, 26, false, true)
	src := &IMAPSource{host: pool.host, port: pool.port, username: "fixture-user", password: "fixture-password", useTLS: false, maxConns: 2, connTimeout: time.Second, readTimeout: time.Second}
	defer src.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	var failure error
	for messages != nil || failures != nil {
		select {
		case _, ok := <-messages:
			if !ok {
				messages = nil
			}
		case err, ok := <-failures:
			if !ok {
				failures = nil
			} else if err != nil {
				failure = err
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if failure == nil || !strings.Contains(failure.Error(), "server policy rejection") {
		t.Fatalf("fetch error = %v, want tagged NO rejection", failure)
	}
	if n := len(pool.accepted); n != 2 {
		t.Fatalf("opened %d IMAP sessions, want no reconnect after tagged NO; error: %v", n, failure)
	}
}
