package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
)

const combinedMessageLimit = 1024
const combinedUIDL = "+OK\r\n1 stable\r\n.\r\n"

func serveReadAheadCommands(server net.Conn, raw string) error {
	reader := bufio.NewReader(server)
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if line != "UIDL\r\n" {
		return fmt.Errorf("first command %q", line)
	}
	// Send together so the shared reader prefetches future status/body bytes.
	// LIST understates size, leaving the decoded body counter authoritative.
	if _, err := io.WriteString(server, combinedUIDL+"+OK 1 1\r\n+OK\r\n"+raw+".\r\n"); err != nil {
		return err
	}
	return expectReadAheadCommands(reader, []string{"LIST 1\r\n", "RETR 1\r\n"})
}

func expectReadAheadCommands(reader *bufio.Reader, wants []string) error {
	for _, want := range wants {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line != want {
			return fmt.Errorf("command %q, want %q", line, want)
		}
	}
	return nil
}

func openReadAheadPipe(t *testing.T, raw string) (*pop3Protocol, <-chan error) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	if err := client.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serveReadAheadCommands(server, raw) }()
	limits := listingLimits{items: 1, bytes: len(combinedUIDL)}
	return &pop3Protocol{conn: client, reader: bufio.NewReader(client), limits: limits}, done
}

func checkReadAheadListing(t *testing.T, conn *pop3Protocol, raw string) {
	t.Helper()
	identities, err := readPOP3UIDLs(conn, 1, conn.limits)
	if err != nil || len(identities) != 1 || identities[0].UID != "stable" {
		t.Fatalf("listing at byte budget: %v, %v", identities, err)
	}
	if conn.reader.Buffered() <= len(raw) {
		t.Fatalf("fixture did not prefetch body/status: %d", conn.reader.Buffered())
	}
}

func checkReadAheadBody(t *testing.T, conn *pop3Protocol, raw string, extra int) {
	t.Helper()
	got, err := conn.retrieve(1, combinedMessageLimit)
	if extra == 0 {
		if err != nil || string(got) != raw {
			t.Fatalf("body larger than listing budget: length=%d, err=%v", len(got), err)
		}
		return
	}
	if !errors.Is(err, plugins.ErrMessageTooLarge) {
		t.Fatalf("prefetched body bypassed message budget: %v", err)
	}
}

func TestPOP3ReadAheadPreservesListingAndMessageBudgets(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(strconv.Itoa(extra), func(t *testing.T) {
			raw := strings.Repeat("x", combinedMessageLimit-2+extra) + "\r\n"
			conn, done := openReadAheadPipe(t, raw)
			checkReadAheadListing(t, conn, raw)
			checkReadAheadBody(t, conn, raw, extra)
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
			err := retryFetch(t.Context(), make(chan *plugins.Message), func(context.Context, chan<- *plugins.Message) error {
				attempts++
				return errors.Join(limitErr, io.ErrUnexpectedEOF, &net.OpError{Op: "read", Net: "tcp", Err: net.ErrClosed})
			})
			if attempts != 1 || !errors.Is(err, limitErr) {
				t.Fatalf("attempts=%d err=%v", attempts, err)
			}
		})
	}
}
