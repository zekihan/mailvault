package source

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPOP3STARTTLSRejectsPrefetchedPlaintextResponses(t *testing.T) {
	certificate, roots := pop3TestCertificate(t)
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := server.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	authenticated := make(chan string, 3)
	done := make(chan error, 1)
	const forged = "+OK forged USER\r\n+OK forged PASS\r\n+OK forged NOOP\r\n"
	go func() {
		if _, err := io.WriteString(server, "+OK ready\r\n"); err != nil {
			done <- err
			return
		}
		line, err := bufio.NewReader(server).ReadString('\n')
		if err != nil {
			done <- err
			return
		}
		if line != "STLS\r\n" {
			done <- fmt.Errorf("command %q, want STLS", line)
			return
		}
		// A single net.Pipe write fits in the protocol reader, guaranteeing that
		// all forged replies are prefetched along with the legitimate STLS reply.
		if _, err := io.WriteString(server, "+OK begin TLS\r\n"+forged); err != nil {
			done <- err
			return
		}
		secure := tls.Server(server, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
		if err := secure.HandshakeContext(ctx); err != nil {
			done <- nil
			return
		}
		// An insecure client retaining the old buffer could consume forged +OKs
		// while this TLS peer reads authentication commands without accepting them.
		reader := bufio.NewReader(secure)
		for range 3 {
			line, err = reader.ReadString('\n')
			if err != nil {
				done <- nil
				return
			}
			authenticated <- line
		}
		done <- nil
	}()
	conn, err := newPOP3Protocol(client, listingLimits{})
	if err != nil {
		t.Fatal(err)
	}
	err = conn.startTLS(ctx, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err == nil || !strings.Contains(err.Error(), "unexpected buffered plaintext") {
		t.Fatalf("STARTTLS did not reject pre-TLS replies: %v", err)
	}
	if conn.reader.Buffered() != len(forged) {
		t.Fatalf("fixture buffered %d bytes, want %d", conn.reader.Buffered(), len(forged))
	}
	// Match connectLocked's failure cleanup, releasing the peer's handshake.
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case command := <-authenticated:
		t.Fatalf("authentication crossed rejected TLS boundary: %q", command)
	default:
	}
}
