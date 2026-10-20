package source

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPOP3RejectsSTARTTLS(t *testing.T) {
	_, err := NewPOP3Source(map[string]interface{}{"host": "example.invalid", "username": "test-user", "password_ref": "synthetic-value", "use_tls": false, "start_tls": true})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("unsupported STARTTLS accepted: %v", err)
	}
}
func TestPOP3RejectsDateFiltersBeforeConnecting(t *testing.T) {
	accepted := make(chan struct{}, 1)
	host, port := protocolServer(t, func(net.Conn, <-chan struct{}) { accepted <- struct{}{} })
	for _, bounds := range [][2]int64{{1, 0}, {0, 2}, {1, 2}} {
		src := &POP3Source{host: host, port: port, username: "test-user", password: "synthetic-value", connTimeout: time.Second, readTimeout: time.Second}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		messages, errs := src.FetchMessages(ctx, []string{"INBOX"}, bounds[0], bounds[1])
		for range messages {
			t.Error("filtered fetch emitted a message")
		}
		var failure error
		for err := range errs {
			failure = err
		}
		cancel()
		if failure == nil || !strings.Contains(failure.Error(), "date filters") {
			t.Fatalf("missing unsupported-filter error: %v", failure)
		}
		select {
		case <-accepted:
			t.Fatal("unsupported filter connected to server")
		default:
		}
	}
}
