package source

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestIMAPCloseAfterFolderFailureAndReconnect(t *testing.T) {
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			if len(parts) < 2 {
				return
			}
			switch parts[1] {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", parts[0])
			case "CAPABILITY":
				_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", parts[0])
			case "EXAMINE", "SELECT":
				_, _ = fmt.Fprintf(conn, "%s NO Archive doesn't exist.\r\n", parts[0])
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected command\r\n", parts[0])
			}
		}
	})
	src := &IMAPSource{host: host, port: port, username: "test", password: "synthetic", connTimeout: time.Second, readTimeout: time.Second}
	t.Cleanup(func() { _ = src.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	messages, failures := src.FetchMessages(ctx, []string{"Archive"}, 0, 0)
	for range messages {
		t.Error("unexpected message")
	}
	var errs []error
	for err := range failures {
		errs = append(errs, err)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "Archive doesn't exist") {
		t.Fatalf("folder error lost: %v", errs)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("cleanup after folder failure: %v", err)
	}
	if err := src.Connect(ctx); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("close reconnected source: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
}
