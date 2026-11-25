// Package mailtest provides loopback protocol fixtures for integration tests.
package mailtest

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// ProtocolServer serves test conversations on loopback until test cleanup.
func ProtocolServer(t *testing.T, serve func(net.Conn, <-chan struct{})) (string, int) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var connections []net.Conn
	wg.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			select {
			case <-stop:
				mu.Unlock()
				_ = conn.Close()
				return
			default:
			}
			connections = append(connections, conn)
			mu.Unlock()
			wg.Go(func() { defer conn.Close(); serve(conn, stop) })
		}
	})
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
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return "127.0.0.1", 0
	}
	return "127.0.0.1", addr.Port
}

// POP3Message is a synthetic message served by POP3Server.
type POP3Message struct {
	UID    string
	Number int
	Raw    string
}

// POP3Server serves a synthetic mailbox and optionally records RETR sequence numbers.
func POP3Server(t *testing.T, messages []POP3Message, retr chan<- int, uidlOrder ...int) (string, int) {
	t.Helper()
	return ProtocolServer(t, func(conn net.Conn, stop <-chan struct{}) {
		_, _ = fmt.Fprint(conn, "+OK fixture ready\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			if len(parts) == 0 {
				return
			}
			if !dispatchPOP3Command(conn, parts, messages, retr, stop, uidlOrder) {
				return
			}
		}
	})
}

func dispatchPOP3Command(conn net.Conn, parts []string, messages []POP3Message, retr chan<- int, stop <-chan struct{}, uidlOrder []int) bool {
	switch strings.ToUpper(parts[0]) {
	case "USER", "PASS", "AUTH", "NOOP":
		_, _ = fmt.Fprint(conn, "+OK accepted\r\n")
	case "STAT":
		_, _ = fmt.Fprintf(conn, "+OK %d 100\r\n", len(messages))
	case "UIDL":
		writePOP3UIDL(conn, messages, uidlOrder)
	case "LIST":
		return writePOP3LIST(conn, parts, messages)
	case "RETR":
		return writePOP3RETR(conn, parts, messages, retr, stop)
	case "QUIT":
		_, _ = fmt.Fprint(conn, "+OK bye\r\n")
		return false
	default:
		_, _ = fmt.Fprint(conn, "-ERR unexpected command\r\n")
	}
	return true
}

func writePOP3UIDL(conn net.Conn, messages []POP3Message, uidlOrder []int) {
	_, _ = fmt.Fprintf(conn, "+OK identifiers\r\n")
	order := uidlOrder
	if len(order) == 0 {
		order = make([]int, len(messages))
		for i := range order {
			order[i] = i
		}
	}
	for _, i := range order {
		message := messages[i]
		number := message.Number
		if number == 0 {
			number = i + 1
		}
		_, _ = fmt.Fprintf(conn, "%d %s\r\n", number, message.UID)
	}
	_, _ = fmt.Fprint(conn, ".\r\n")
}

func writePOP3LIST(conn net.Conn, parts []string, messages []POP3Message) bool {
	var id int
	if len(parts) != 2 {
		return false
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &id); err != nil || id < 1 || id > len(messages) {
		return false
	}
	size := len(messages[id-1].Raw)
	if !strings.HasSuffix(messages[id-1].Raw, "\r\n") {
		size += 2
	}
	_, _ = fmt.Fprintf(conn, "+OK %d %d\r\n", id, size)
	return true
}

func writePOP3RETR(conn net.Conn, parts []string, messages []POP3Message, retr chan<- int, stop <-chan struct{}) bool {
	if len(parts) != 2 {
		return false
	}
	var id int
	if _, err := fmt.Sscanf(parts[1], "%d", &id); err != nil || id < 1 || id > len(messages) {
		_, _ = fmt.Fprint(conn, "-ERR no such message\r\n")
		return true
	}
	if retr != nil {
		select {
		case retr <- id:
		case <-stop:
			return false
		}
	}
	_, _ = fmt.Fprint(conn, "+OK message follows\r\n")
	_, _ = fmt.Fprint(conn, messages[id-1].Raw)
	if !strings.HasSuffix(messages[id-1].Raw, "\r\n") {
		_, _ = fmt.Fprint(conn, "\r\n")
	}
	_, _ = fmt.Fprint(conn, ".\r\n")
	return true
}
