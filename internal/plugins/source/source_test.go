package source

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
)

func protocolServer(t *testing.T, serve func(net.Conn, <-chan struct{})) (string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var connections []net.Conn
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections = append(connections, conn)
			mu.Unlock()
			wg.Add(1)
			go func() { defer wg.Done(); defer conn.Close(); serve(conn, stop) }()
		}
	}()
	t.Cleanup(func() {
		close(stop)
		_ = listener.Close()
		mu.Lock()
		for _, conn := range connections {
			_ = conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return "127.0.0.1", listener.Addr().(*net.TCPAddr).Port
}

func TestIMAPFetchUsesUIDsAndInternalDate(t *testing.T) {
	raw := "Message-ID: <fixture@example.invalid>\r\nSubject: test\r\n\r\nbody\r\n"
	commands := make(chan string, 20)
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			line := scanner.Text()
			commands <- line
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return
			}
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
				if parts[2] == "SEARCH" {
					_, _ = fmt.Fprintf(conn, "* SEARCH 42\r\n%s OK searched\r\n", tag)
				} else {
					_, _ = fmt.Fprintf(conn, "* 1 FETCH (UID 42 INTERNALDATE \"01-Jan-2024 00:00:00 +0000\" BODY[] {%d}\r\n%s)\r\n%s OK fetched\r\n", len(raw), raw, tag)
				}
			case "SEARCH":
				_, _ = fmt.Fprintf(conn, "* SEARCH 1\r\n%s OK searched\r\n", tag)
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
			}
		}
	})
	src := &IMAPSource{host: host, port: port, username: "test-user", password: "test-value", connTimeout: time.Second, readTimeout: time.Second, maxConns: 5}
	defer src.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
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
	if len(got) != 1 || got[0].UID != "42" || string(got[0].Raw) != raw || got[0].InternalDate != "2024-01-01T00:00:00Z" {
		t.Fatalf("unexpected messages: %+v", got)
	}
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
	folders, err := src.ListFolders(ctx)
	if err != nil || len(folders) != 1 || folders[0] != "INBOX" {
		t.Fatalf("ListFolders() = %v, %v", folders, err)
	}
}

func TestIMAPFetchReconnectsAfterTruncatedMessage(t *testing.T) {
	raw := "Message-ID: <recovered@example.invalid>\r\nSubject: recovered\r\n\r\ncomplete body\r\n"
	var mu sync.Mutex
	connections, fetches := 0, 0
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		mu.Lock()
		connections++
		connection := connections
		mu.Unlock()
		_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
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
				_, _ = fmt.Fprintf(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n%s OK [READ-ONLY] selected\r\n", tag)
			case "UID":
				if len(parts) < 3 {
					return
				}
				switch strings.ToUpper(parts[2]) {
				case "SEARCH":
					_, _ = fmt.Fprintf(conn, "* SEARCH 42\r\n%s OK searched\r\n", tag)
				case "FETCH":
					mu.Lock()
					fetches++
					mu.Unlock()
					if connection == 1 {
						_, _ = fmt.Fprintf(conn, "* 1 FETCH (UID 42 BODY[] {%d}\r\n%s", len(raw), raw[:len(raw)/2])
						return
					}
					_, _ = fmt.Fprintf(conn, "* 1 FETCH (UID 42 INTERNALDATE \"01-Jan-2024 00:00:00 +0000\" BODY[] {%d}\r\n%s)\r\n%s OK fetched\r\n", len(raw), raw, tag)
				}
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
			}
		}
	})
	src := &IMAPSource{host: host, port: port, username: "test-user", password: "test-password", connTimeout: time.Second, readTimeout: time.Second, maxConns: 1}
	defer src.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
	mu.Lock()
	gotConnections, gotFetches := connections, fetches
	mu.Unlock()
	if gotConnections != 2 || gotFetches != 2 {
		if len(got) > 0 {
			t.Fatalf("connections=%d fetches=%d; want reconnect and retry, got UID=%q raw=%q", gotConnections, gotFetches, got[0].UID, got[0].Raw)
		}
		t.Fatalf("connections=%d fetches=%d; want reconnect and retry, got messages %+v", gotConnections, gotFetches, got)
	}
	if len(got) != 1 || string(got[0].Raw) != raw || got[0].UID != "42" {
		t.Fatalf("recovered messages = %+v", got)
	}
}

func TestSourceDurationConfig(t *testing.T) {
	values := map[string]interface{}{
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
	if imapSource.(*IMAPSource).readTimeout != 60*time.Millisecond || popSource.(*POP3Source).readTimeout != 60*time.Millisecond {
		t.Fatal("typed configuration durations were discarded")
	}
}

func TestSourceReadTimeoutAndCancellation(t *testing.T) {
	for _, kind := range []string{"imap", "pop3"} {
		for _, phase := range []string{"greeting", "auth", "fetch"} {
			for _, cancelOperation := range []bool{false, true} {
				t.Run(kind+"/"+phase+"/cancel="+strconv.FormatBool(cancelOperation), func(t *testing.T) {
					reached := make(chan struct{}, 1)
					host, port := protocolServer(t, func(conn net.Conn, stop <-chan struct{}) {
						stall := func() {
							select {
							case reached <- struct{}{}:
							default:
							}
							<-stop
						}
						if phase == "greeting" {
							stall()
							return
						}
						if kind == "imap" {
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
							if kind == "pop3" {
								if (phase == "auth" && parts[0] == "USER") || (phase == "fetch" && parts[0] == "STAT") {
									stall()
									return
								}
								_, _ = fmt.Fprint(conn, "+OK\r\n")
							} else {
								if len(parts) < 2 {
									return
								}
								if (phase == "auth" && parts[1] == "LOGIN") || (phase == "fetch" && parts[1] == "EXAMINE") {
									stall()
									return
								}
								if parts[1] == "CAPABILITY" {
									_, _ = fmt.Fprint(conn, "* CAPABILITY IMAP4rev1\r\n")
								}
								_, _ = fmt.Fprintf(conn, "%s OK done\r\n", parts[0])
							}
						}
					})
					readTimeout := 60 * time.Millisecond
					if cancelOperation {
						readTimeout = 5 * time.Second
					}
					var src plugins.Source
					if kind == "imap" {
						src = &IMAPSource{host: host, port: port, username: "test-user", password: "test-value", connTimeout: time.Second, readTimeout: readTimeout, maxConns: 1}
					} else {
						src = &POP3Source{host: host, port: port, username: "test-user", password: "test-value", connTimeout: time.Second, readTimeout: readTimeout}
					}
					ctx, cancel := context.WithCancel(context.Background())
					t.Cleanup(cancel)
					done := make(chan error, 1)
					go func() {
						if phase != "fetch" {
							done <- src.Connect(ctx)
							return
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
						done <- failure
					}()
					select {
					case <-reached:
					case <-time.After(time.Second):
						t.Fatal("operation did not reach stalled server")
					}
					if cancelOperation {
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
				})
			}
		}
	}
}
