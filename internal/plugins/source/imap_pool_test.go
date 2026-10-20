package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func imapPoolFixture(t *testing.T, count int, blockFetch bool) (string, int, <-chan struct{}, <-chan struct{}, func() int, func(map[string]int)) {
	t.Helper()
	accepted := make(chan struct{}, 8)
	fetchStarted := make(chan struct{}, 8)
	var mu sync.Mutex
	active, maxActive := 0, 0
	got := make(map[string]int)
	host, port := protocolServer(t, func(conn net.Conn, stop <-chan struct{}) {
		accepted <- struct{}{}
		_, _ = fmt.Fprint(conn, "* OK fixture ready\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			line := scanner.Text()
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return
			}
			tag := parts[0]
			switch strings.ToUpper(parts[1]) {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
			case "CAPABILITY":
				_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", tag)
			case "EXAMINE":
				_, _ = fmt.Fprintf(conn, "* %d EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n* OK [UIDNEXT %d] next\r\n%s OK [READ-ONLY] selected\r\n", count, count+1, tag)
			case "UID":
				if len(parts) < 4 {
					_, _ = fmt.Fprintf(conn, "%s BAD incomplete UID command\r\n", tag)
					continue
				}
				switch strings.ToUpper(parts[2]) {
				case "SEARCH":
					_, _ = fmt.Fprint(conn, "* SEARCH")
					for i := 1; i <= count; i++ {
						_, _ = fmt.Fprintf(conn, " %d", i)
					}
					_, _ = fmt.Fprintf(conn, "\r\n%s OK searched\r\n", tag)
				case "FETCH":
					mu.Lock()
					active++
					if active > maxActive {
						maxActive = active
					}
					mu.Unlock()
					select {
					case fetchStarted <- struct{}{}:
					default:
					}
					if blockFetch {
						<-stop
					} else {
						time.Sleep(25 * time.Millisecond)
					}
					ids, err := parseUIDSet(parts[3])
					if err != nil {
						mu.Lock()
						active--
						mu.Unlock()
						_, _ = fmt.Fprintf(conn, "%s BAD invalid uid set\r\n", tag)
						continue
					}
					for _, id := range ids {
						raw := fmt.Sprintf("Message-ID: <pool-%d@example.invalid>\r\nSubject: %d\r\n\r\nbody-%d\r\n", id, id, id)
						_, _ = fmt.Fprintf(conn, "* %d FETCH (UID %d INTERNALDATE \"01-Jan-2024 00:00:00 +0000\" FLAGS (\\Seen \\Flagged) BODY[] {%d}\r\n%s)\r\n", id, id, len(raw), raw)
						mu.Lock()
						got[strconv.Itoa(id)]++
						mu.Unlock()
					}
					_, _ = fmt.Fprintf(conn, "%s OK fetched\r\n", tag)
					mu.Lock()
					active--
					mu.Unlock()
				}
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
			}
		}
	})
	maxFn := func() int { mu.Lock(); defer mu.Unlock(); return maxActive }
	gotFn := func(dst map[string]int) {
		mu.Lock()
		defer mu.Unlock()
		for id, n := range got {
			dst[id] = n
		}
	}
	return host, port, accepted, fetchStarted, maxFn, gotFn
}

func parseUIDSet(value string) ([]int, error) {
	value = strings.Trim(value, "(")
	value = strings.TrimSuffix(value, ")")
	var ids []int
	for _, part := range strings.Split(value, ",") {
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
	host, port, accepted, _, maxConcurrent, fetches := imapPoolFixture(t, count, false)
	src := &IMAPSource{host: host, port: port, username: "fixture-user", password: "fixture-password", useTLS: false, maxConns: 2, connTimeout: time.Second, readTimeout: time.Second}
	defer src.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
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
	if gotMessages != count || maxConcurrent() != 2 {
		t.Fatalf("messages=%d max concurrent FETCH=%d, want %d and 2", gotMessages, maxConcurrent(), count)
	}
	if n := len(accepted); n != 2 {
		t.Fatalf("opened %d IMAP sessions, want 2", n)
	}
	seen := make(map[string]int)
	fetches(seen)
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
	host, port, _, fetchStarted, _, _ := imapPoolFixture(t, 50, true)
	src := &IMAPSource{host: host, port: port, username: "fixture-user", password: "fixture-password", useTLS: false, maxConns: 2, connTimeout: time.Second, readTimeout: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	select {
	case <-fetchStarted:
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
