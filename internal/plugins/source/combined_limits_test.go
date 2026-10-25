package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestPOP3ReadAheadPreservesListingAndMessageBudgets(t *testing.T) {
	const messageLimit = 1024
	const uidl = "+OK\r\n1 stable\r\n.\r\n"
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			if err := client.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			raw := strings.Repeat("x", messageLimit-2+extra) + "\r\n"
			done := make(chan error, 1)
			go func() {
				reader := bufio.NewReader(server)
				line, err := reader.ReadString('\n')
				if err != nil {
					done <- err
					return
				}
				if line != "UIDL\r\n" {
					done <- fmt.Errorf("first command %q", line)
					return
				}
				// Send together so the shared reader prefetches future status/body bytes.
				// LIST understates size, leaving the decoded body counter authoritative.
				_, err = io.WriteString(server, uidl+"+OK 1 1\r\n+OK\r\n"+raw+".\r\n")
				if err != nil {
					done <- err
					return
				}
				for _, want := range []string{"LIST 1\r\n", "RETR 1\r\n"} {
					line, err = reader.ReadString('\n')
					if err != nil {
						done <- err
						return
					}
					if line != want {
						done <- fmt.Errorf("command %q, want %q", line, want)
						return
					}
				}
				done <- nil
			}()
			limits := listingLimits{items: 1, bytes: len(uidl)}
			conn := &pop3Protocol{conn: client, reader: bufio.NewReader(client), limits: limits}
			identities, err := readPOP3UIDLs(conn, 1, limits)
			if err != nil || len(identities) != 1 || identities[0].UID != "stable" {
				t.Fatalf("listing at byte budget: %v, %v", identities, err)
			}
			if conn.reader.Buffered() <= len(raw) {
				t.Fatalf("fixture did not prefetch body/status: %d", conn.reader.Buffered())
			}
			got, err := conn.retrieve(1, messageLimit)
			if extra == 0 {
				if err != nil || string(got) != raw {
					t.Fatalf("body larger than listing budget: length=%d, err=%v", len(got), err)
				}
			} else if !errors.Is(err, plugins.ErrMessageTooLarge) {
				t.Fatalf("prefetched body bypassed message budget: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLimitErrorsNeverRetryAfterTransportClosure(t *testing.T) {
	for _, limitErr := range []error{errListingLimit, plugins.ErrMessageTooLarge} {
		t.Run(limitErr.Error(), func(t *testing.T) {
			attempts := 0
			err := retryFetch(context.Background(), make(chan *plugins.Message), func(context.Context, chan<- *plugins.Message) error {
				attempts++
				return errors.Join(limitErr, io.ErrUnexpectedEOF, &net.OpError{Op: "read", Net: "tcp", Err: net.ErrClosed})
			})
			if attempts != 1 || !errors.Is(err, limitErr) {
				t.Fatalf("attempts=%d err=%v", attempts, err)
			}
		})
	}
}
