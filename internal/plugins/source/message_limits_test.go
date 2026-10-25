package source

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestIMAPRejectsOversizedLiteralWithoutReadingBody(t *testing.T) {
	for _, size := range []int64{1025, 1 << 40, 1026} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
				fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					parts := strings.Fields(scanner.Text())
					if len(parts) < 2 {
						return
					}
					switch parts[1] {
					case "LOGIN":
						fmt.Fprintf(conn, "%s OK authenticated\r\n", parts[0])
					case "CAPABILITY":
						fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", parts[0])
					case "EXAMINE":
						fmt.Fprintf(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n%s OK selected\r\n", parts[0])
					case "UID":
						if parts[2] == "SEARCH" {
							fmt.Fprintf(conn, "* SEARCH 1\r\n%s OK searched\r\n", parts[0])
						} else {
							// Only the size is sent. A body read would block until the test deadline.
							fmt.Fprintf(conn, "* 1 FETCH (UID 1 BODY[] {%d}\r\n", size)
							if size == 1026 {
								// Exercise cleanup when response bytes are already buffered,
								// including enough extra items to block an undrained decoder.
								fmt.Fprint(conn, strings.Repeat("x", int(size)), strings.Repeat(" FLAGS ()", 40), " BODY[] {1}\r\nx)\r\n")
							}

						}
					}
				}
			})
			src := &IMAPSource{host: host, port: port, username: "fixture", password: "synthetic", maxConns: 1, maxMessageBytes: 1024, connTimeout: time.Second, readTimeout: 5 * time.Second}
			defer src.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
			for range messages {
				t.Fatal("oversized message emitted")
			}
			var failure error
			for err := range failures {
				failure = err
			}
			if !errors.Is(failure, plugins.ErrMessageTooLarge) {
				t.Fatalf("want size rejection before body read, got %v", failure)
			}
		})
	}
}

func TestPOP3RejectsUnadvertisedOverflowAndLISTOversize(t *testing.T) {
	for _, advertised := range []int{1, 65} {
		t.Run(fmt.Sprint(advertised), func(t *testing.T) {
			retr := make(chan struct{}, 1)
			host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
				fmt.Fprint(conn, "+OK ready\r\n")
				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					parts := strings.Fields(scanner.Text())
					switch parts[0] {
					case "USER", "PASS", "NOOP":
						fmt.Fprint(conn, "+OK\r\n")
					case "STAT":
						fmt.Fprint(conn, "+OK 1 1\r\n")
					case "UIDL":
						fmt.Fprint(conn, "+OK\r\n1 stable\r\n.\r\n")
					case "LIST":
						fmt.Fprintf(conn, "+OK 1 %d\r\n", advertised)
					case "RETR":
						retr <- struct{}{}
						// No terminator: the reader must abort a responsive, endless body.
						fmt.Fprint(conn, "+OK\r\n", strings.Repeat("x", 8192))
					}
				}
			})
			src := &POP3Source{host: host, port: port, username: "fixture", password: "synthetic", maxMessageBytes: 64, connTimeout: time.Second, readTimeout: 5 * time.Second}
			defer src.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
			for range messages {
				t.Fatal("oversized message emitted")
			}
			var failure error
			for err := range failures {
				failure = err
			}
			if !errors.Is(failure, plugins.ErrMessageTooLarge) {
				t.Fatalf("expected size rejection, got %v", failure)
			}
			if (len(retr) > 0) != (advertised == 1) {
				t.Fatalf("RETR count=%d for advertised=%d", len(retr), advertised)
			}
		})
	}
}

func TestPOP3BoundedBodyPreservesBytesAcrossFragments(t *testing.T) {
	for _, raw := range []string{
		"Subject: test\r\n\r\n..dot-stuffed\r\nbody\r\n",
		strings.Repeat("x", 4095) + "\r\n", // CR/LF split across the reader boundary
		strings.Repeat("x", 8192) + "\r\n",
	} {
		want := strings.ReplaceAll(raw, "..dot-stuffed", ".dot-stuffed")
		conn := &pop3Protocol{reader: bufio.NewReader(strings.NewReader(raw + ".\r\n"))}
		got, err := conn.readBody(int64(len(want)))
		if err != nil || !bytes.Equal(got, []byte(want)) {
			t.Fatalf("body length %d: got %d bytes, err=%v", len(want), len(got), err)
		}
		conn.reader = bufio.NewReader(strings.NewReader(raw + ".\r\n"))
		if _, err := conn.readBody(int64(len(want) - 1)); !errors.Is(err, plugins.ErrMessageTooLarge) {
			t.Fatalf("one-byte overflow accepted: %v", err)
		}
	}
}

func TestSourcesApplyBackpressureBehindBlockedTarget(t *testing.T) {
	for _, kind := range []string{"imap", "pop3"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			fetched := make(chan int, 100)
			ctx = plugins.WithFetchProgress(ctx, func(progress plugins.FetchProgress) {
				if progress.Fetched > 0 {
					fetched <- progress.Fetched
				}
			})
			var src plugins.Source
			limit := int64(len(fmt.Sprintf("Message-ID: <pool-%d@example.invalid>\r\nSubject: %d\r\n\r\nbody-%d\r\n", 50, 50, 50)))
			workers := 2
			if kind == "imap" {
				host, port, _, _, _, _ := imapPoolFixture(t, 50, false)
				src = &IMAPSource{host: host, port: port, username: "fixture", password: "synthetic", maxConns: workers, maxMessageBytes: limit, connTimeout: time.Second, readTimeout: time.Second}
			} else {
				workers = 1
				fixture := make([]pop3FixtureMessage, 50)
				for i := range fixture {
					fixture[i] = pop3FixtureMessage{uid: fmt.Sprint(i), raw: strings.Repeat("x", int(limit)-2) + "\r\n"}
				}
				host, port := pop3Fixture(t, fixture, nil)
				src = &POP3Source{host: host, port: port, username: "fixture", password: "synthetic", maxMessageBytes: limit, connTimeout: time.Second, readTimeout: time.Second}
			}
			defer func() { cancel(); _ = src.Close() }()
			messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
			// The target consumes one message and blocks. At most one per worker plus
			// one in the retry relay can remain resident, besides the target's message.
			select {
			case <-messages:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			maxFetched := workers + 2
			for n := 0; n < maxFetched; n++ {
				select {
				case <-fetched:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			select {
			case n := <-fetched:
				t.Fatalf("fetched %d while target blocked; raw byte budget is %d", n, int64(maxFetched)*limit)
			case <-time.After(100 * time.Millisecond):
			}
			if cap(messages) != 0 {
				t.Fatalf("source queue capacity=%d", cap(messages))
			}
			cancel()
			for range messages {
			}
			for range failures {
			}
		})
	}
}

func TestRestoreAppendRejectsOversizeBeforeConnecting(t *testing.T) {
	src := &IMAPSource{maxMessageBytes: 4}
	err := src.AppendMessage(context.Background(), "Restored", &plugins.Message{Raw: []byte("large")}, "7")
	if !errors.Is(err, plugins.ErrMessageTooLarge) {
		t.Fatalf("oversized APPEND reached transport: %v", err)
	}
}
