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

func TestIMAPReconnectRefreshesOnlyDeliveredUIDs(t *testing.T) {
	const raw = "Subject: synthetic fixture\r\n\r\nbody\r\n"
	var connections atomic.Int32
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		attempt := connections.Add(1)
		_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			tag := parts[0]
			switch parts[1] {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
			case "EXAMINE":
				_, _ = fmt.Fprintf(conn, "* 3 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n%s OK selected\r\n", tag)
			case "UID":
				switch parts[2] {
				case "SEARCH":
					search := "2 3"
					if attempt == 2 {
						if !strings.HasSuffix(scanner.Text(), "NOT (UID 1:2)") {
							t.Errorf("retry search failed to exclude delivered and streamed UIDs: %s", scanner.Text())
						}
						search = "3"
					}
					_, _ = fmt.Fprintf(conn, "* SEARCH %s\r\n%s OK searched\r\n", search, tag)
				case "FETCH":
					if !strings.Contains(scanner.Text(), "BODY[") {
						ids, err := parseUIDSet(parts[3])
						if err != nil {
							t.Error(err)
							return
						}
						for _, uid := range ids {
							_, _ = fmt.Fprintf(conn, "* %d FETCH (UID %d FLAGS (\\Seen))\r\n", uid, uid)
						}
						_, _ = fmt.Fprintf(conn, "%s OK flags sent\r\n", tag)
						continue
					}
					if attempt == 1 {
						_, _ = fmt.Fprintf(conn, "* 2 FETCH (UID 2 BODY[] {%d}\r\n%s)\r\n", len(raw), raw)
						return
					}
					if parts[3] != "3" {
						t.Errorf("retry body fetch = %q, want only UID 3", parts[3])
					}
					_, _ = fmt.Fprintf(conn, "* 3 FETCH (UID 3 BODY[] {%d}\r\n%s)\r\n%s OK fetched\r\n", len(raw), raw, tag)
				}
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
			}
		}
	})
	src := &IMAPSource{host: host, port: port, username: "fixture", password: "fixture", connTimeout: time.Second, readTimeout: time.Second, maxConns: 1}
	defer src.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = plugins.WithKnownUIDs(ctx, func(string, string) (map[string]bool, error) {
		return map[string]bool{"1": true}, nil
	})
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
	if !slices.Equal(fetched, []string{"2", "3"}) || !slices.Equal(refreshed, []string{"1", "1"}) || connections.Load() != 2 {
		t.Fatalf("fetched %v, refreshed %v, connections %d", fetched, refreshed, connections.Load())
	}
}

func TestMetadataRefreshDoesNotFetchBody(t *testing.T) {
	commands := make(chan string, 10)
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			line := scanner.Text()
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return
			}
			tag := parts[0]
			switch parts[1] {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
			case "EXAMINE":
				_, _ = fmt.Fprintf(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n%s OK selected\r\n", tag)
			case "UID":
				commands <- line
				if parts[2] == "FETCH" {
					_, _ = fmt.Fprintf(conn, "* 1 FETCH (UID 42 FLAGS (\\Seen custom))\r\n%s OK fetched\r\n", tag)
				} else {
					_, _ = fmt.Fprintf(conn, "* SEARCH\r\n%s OK searched\r\n", tag)
				}
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
			}
		}
	})
	src := &IMAPSource{host: host, port: port, username: "test", password: "test", connTimeout: time.Second, readTimeout: time.Second, maxConns: 1}
	defer src.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	for len(commands) > 0 {
		command := <-commands
		if strings.Contains(command, "FETCH") && strings.Contains(command, "BODY") {
			t.Fatalf("body fetched: %s", command)
		}
	}
}

func TestMetadataRefreshCancellationInterruptsBlockedFlagsFetch(t *testing.T) {
	fetchStarted := make(chan struct{}, 1)
	host, port := protocolServer(t, func(conn net.Conn, stop <-chan struct{}) {
		_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			if len(parts) < 2 {
				return
			}
			tag := parts[0]
			switch parts[1] {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
			case "EXAMINE":
				_, _ = fmt.Fprintf(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n%s OK selected\r\n", tag)
			case "UID":
				if len(parts) > 2 && parts[2] == "FETCH" {
					fetchStarted <- struct{}{}
					<-stop
					return
				}
				_, _ = fmt.Fprintf(conn, "* SEARCH\r\n%s OK searched\r\n", tag)
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
			}
		}
	})
	src := &IMAPSource{host: host, port: port, username: "fixture", password: "fixture", connTimeout: time.Second, readTimeout: 10 * time.Second, maxConns: 1}
	defer src.Close()
	ctx, cancel := context.WithCancel(context.Background())
	ctx = plugins.WithKnownUIDs(ctx, func(string, string) (map[string]bool, error) { return map[string]bool{"42": true}, nil })
	ctx = plugins.WithMetadataObserver(ctx, func(*plugins.Message) error { return nil })
	messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	select {
	case <-fetchStarted:
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
