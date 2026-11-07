package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var fastConnectRetry = connectBackoff{budget: 2 * time.Second, initial: 10 * time.Millisecond, max: 50 * time.Millisecond}

// serveIMAPLogin answers LOGIN with the given tagged response suffix.
func serveIMAPLogin(conn net.Conn, login string) {
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
			_, _ = fmt.Fprintf(conn, "%s %s\r\n", tag, login)
		case "LOGOUT":
			_, _ = fmt.Fprintf(conn, "* BYE closing\r\n%s OK logout\r\n", tag)
			return
		default:
			_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
		}
	}
}

func retryTestSource(host string, port int) *IMAPSource {
	return &IMAPSource{name: "retry-fixture", host: host, port: port, username: "fixture-user", password: "fixture-password",
		connTimeout: time.Second, readTimeout: time.Second, connectRetry: fastConnectRetry}
}

func TestIMAPConnectRetriesDroppedConnections(t *testing.T) {
	var connections atomic.Int32
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		if connections.Add(1) <= 3 {
			return
		}
		serveIMAPLogin(conn, "OK logged in")
	})
	src := retryTestSource(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := src.Connect(ctx); err != nil {
		t.Fatalf("connect after dropped connections: %v", err)
	}
	defer src.Close()
	if got := connections.Load(); got != 4 {
		t.Fatalf("connections=%d, want 4", got)
	}
}

func TestIMAPConnectRetriesRefusedConnections(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserved.Addr().String()
	port := reserved.Addr().(*net.TCPAddr).Port
	_ = reserved.Close()

	listening := make(chan net.Listener, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			listening <- nil
			return
		}
		listening <- listener
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		serveIMAPLogin(conn, "OK logged in")
	}()
	t.Cleanup(func() {
		if listener := <-listening; listener != nil {
			_ = listener.Close()
		}
	})

	src := retryTestSource("127.0.0.1", port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := src.Connect(ctx); err != nil {
		t.Fatalf("connect after refused connections: %v", err)
	}
	_ = src.Close()
}

func TestIMAPConnectUnreachableFailsWithinBudget(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reserved.Addr().(*net.TCPAddr).Port
	_ = reserved.Close()

	src := retryTestSource("127.0.0.1", port)
	src.connectRetry.budget = 300 * time.Millisecond
	started := time.Now()
	err = src.Connect(context.Background())
	elapsed := time.Since(started)
	if !errors.Is(err, errConnectRetriesExhausted) {
		t.Fatalf("err=%v, want exhausted retries", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("unreachable server took %v, beyond the retry budget", elapsed)
	}
	if transientSourceError(err) {
		t.Fatal("exhausted connection retries would be retried again by the fetch loop")
	}
}

func TestIMAPConnectDoesNotRetryAuthRejection(t *testing.T) {
	var connections atomic.Int32
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		connections.Add(1)
		serveIMAPLogin(conn, "NO [AUTHENTICATIONFAILED] invalid credentials")
	})
	src := retryTestSource(host, port)
	err := src.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("err=%v, want login rejection", err)
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("auth rejection retried: connections=%d", got)
	}
}

func TestIMAPConnectRetriesUnavailableLogin(t *testing.T) {
	var connections atomic.Int32
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		if connections.Add(1) == 1 {
			serveIMAPLogin(conn, "NO [UNAVAILABLE] backend starting")
			return
		}
		serveIMAPLogin(conn, "OK logged in")
	})
	src := retryTestSource(host, port)
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("connect after temporary login failure: %v", err)
	}
	_ = src.Close()
	if got := connections.Load(); got != 2 {
		t.Fatalf("connections=%d, want 2", got)
	}
}

func TestRetryConnectHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	policy := connectBackoff{budget: 2 * time.Hour, initial: time.Hour}
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	err := retryConnect(ctx, policy, "fixture", func(context.Context) error {
		attempts++
		return io.EOF
	})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}

func TestRetryConnectZeroBudgetMakesOneAttempt(t *testing.T) {
	attempts := 0
	err := retryConnect(context.Background(), connectBackoff{}, "fixture", func(context.Context) error {
		attempts++
		return io.EOF
	})
	if !errors.Is(err, io.EOF) || attempts != 1 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}

func TestRetryConnectBackoffSchedule(t *testing.T) {
	policy := connectBackoff{budget: DefaultConnectRetryTimeout}
	delay, total, waits := connectRetryInitialDelay, time.Duration(0), 0
	for total+delay <= policy.budget {
		total += delay
		waits++
		delay = min(delay*2, connectRetryMaxDelay)
	}
	if total < 150*time.Second || waits < 8 {
		t.Fatalf("default schedule waits %v over %d retries; homelab cold starts need >= 150s", total, waits)
	}
}
