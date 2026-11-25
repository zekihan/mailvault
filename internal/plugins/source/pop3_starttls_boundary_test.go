package source

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type startTLSBoundaryFixture struct {
	certificate   tls.Certificate
	forged        string
	authenticated chan string
	done          chan error
}

func (fixture *startTLSBoundaryFixture) serve(ctx context.Context, server net.Conn) {
	if _, err := io.WriteString(server, "+OK ready\r\n"); err != nil {
		fixture.done <- err
		return
	}
	line, err := bufio.NewReader(server).ReadString('\n')
	if err != nil {
		fixture.done <- err
		return
	}
	if !fixture.expectSTLS(line) {
		return
	}
	fixture.serveTLS(ctx, server)
}

func (fixture *startTLSBoundaryFixture) expectSTLS(line string) bool {
	if line != "STLS\r\n" {
		fixture.done <- fmt.Errorf("command %q, want STLS", line)
		return false
	}
	// A single net.Pipe write fits in the protocol reader, guaranteeing that
	// all forged replies are prefetched along with the legitimate STLS reply.
	return true
}

func (fixture *startTLSBoundaryFixture) serveTLS(ctx context.Context, server net.Conn) {
	if _, err := io.WriteString(server, "+OK begin TLS\r\n"+fixture.forged); err != nil {
		fixture.done <- err
		return
	}
	secure := tls.Server(server, &tls.Config{Certificates: []tls.Certificate{fixture.certificate}, MinVersion: tls.VersionTLS12})
	if err := secure.HandshakeContext(ctx); err != nil {
		fixture.done <- nil
		return
	}
	fixture.readTLSAuth(secure)
}

func (fixture *startTLSBoundaryFixture) readTLSAuth(secure *tls.Conn) {
	// An insecure client retaining the old buffer could consume forged +OKs
	// while this TLS peer reads authentication commands without accepting them.
	reader := bufio.NewReader(secure)
	for range 3 {
		line, err := reader.ReadString('\n')
		if err != nil {
			fixture.done <- nil
			return
		}
		fixture.authenticated <- line
	}
	fixture.done <- nil
}

func openStartTLSBoundaryPipe(t *testing.T) (net.Conn, net.Conn, context.Context, context.CancelFunc) {
	t.Helper()
	client, server := net.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	if err := client.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := server.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		cancel()
		t.Fatal(err)
	}
	return client, server, ctx, cancel
}

func checkStartTLSBoundary(ctx context.Context, t *testing.T, client net.Conn, roots *x509.CertPool, fixture *startTLSBoundaryFixture) {
	t.Helper()
	conn, err := newPOP3Protocol(client, listingLimits{})
	if err != nil {
		t.Fatal(err)
	}
	err = conn.startTLS(ctx, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err == nil || !strings.Contains(err.Error(), "unexpected buffered plaintext") {
		t.Fatalf("STARTTLS did not reject pre-TLS replies: %v", err)
	}
	if conn.reader.Buffered() != len(fixture.forged) {
		t.Fatalf("fixture buffered %d bytes, want %d", conn.reader.Buffered(), len(fixture.forged))
	}
	// Match connectLocked's failure cleanup, releasing the peer's handshake.
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-fixture.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case command := <-fixture.authenticated:
		t.Fatalf("authentication crossed rejected TLS boundary: %q", command)
	default:
	}
}

func TestPOP3STARTTLSRejectsPrefetchedPlaintextResponses(t *testing.T) {
	certificate, roots := pop3TestCertificate(t)
	client, server, ctx, cancel := openStartTLSBoundaryPipe(t)
	defer client.Close()
	defer server.Close()
	defer cancel()
	const forged = "+OK forged USER\r\n+OK forged PASS\r\n+OK forged NOOP\r\n"
	fixture := &startTLSBoundaryFixture{certificate: certificate, forged: forged, authenticated: make(chan string, 3), done: make(chan error, 1)}
	go fixture.serve(ctx, server)
	checkStartTLSBoundary(ctx, t, client, roots, fixture)
}
