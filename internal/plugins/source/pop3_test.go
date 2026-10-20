package source

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

type pop3FixtureMessage struct {
	uid    string
	number int
	raw    string
}

func pop3Fixture(t *testing.T, messages []pop3FixtureMessage, retr chan<- int, uidlOrder ...int) (string, int) {
	t.Helper()
	return protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		_, _ = fmt.Fprint(conn, "+OK fixture ready\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			if len(parts) == 0 {
				return
			}
			switch strings.ToUpper(parts[0]) {
			case "USER", "PASS", "AUTH", "NOOP":
				_, _ = fmt.Fprint(conn, "+OK accepted\r\n")
			case "STAT":
				_, _ = fmt.Fprintf(conn, "+OK %d 100\r\n", len(messages))
			case "UIDL":
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
					number := message.number
					if number == 0 {
						number = i + 1
					}
					_, _ = fmt.Fprintf(conn, "%d %s\r\n", number, message.uid)
				}
				_, _ = fmt.Fprint(conn, ".\r\n")
			case "RETR":
				if len(parts) != 2 {
					return
				}
				var id int
				if _, err := fmt.Sscanf(parts[1], "%d", &id); err != nil || id < 1 || id > len(messages) {
					_, _ = fmt.Fprint(conn, "-ERR no such message\r\n")
					continue
				}
				if retr != nil {
					retr <- id
				}
				_, _ = fmt.Fprint(conn, "+OK message follows\r\n")
				_, _ = fmt.Fprint(conn, messages[id-1].raw)
				if !strings.HasSuffix(messages[id-1].raw, "\r\n") {
					_, _ = fmt.Fprint(conn, "\r\n")
				}
				_, _ = fmt.Fprint(conn, ".\r\n")
			case "QUIT":
				_, _ = fmt.Fprint(conn, "+OK bye\r\n")
				return
			default:
				_, _ = fmt.Fprint(conn, "-ERR unexpected command\r\n")
			}
		}
	})
}

func TestPOP3UIDLIncrementalAndDateFilter(t *testing.T) {
	messages := []pop3FixtureMessage{
		{uid: "newer-id", number: 1, raw: "Message-ID: <new@example.invalid>\r\nDate: Mon, 01 Jan 2024 13:00:00 +0000\r\n\r\nnew\r\n"},
		{uid: "older-id", number: 2, raw: "Message-ID: <old@example.invalid>\r\nDate: Mon, 01 Jan 2024 11:00:00 +0000\r\n\r\nold\r\n"},
	}
	retrieved := make(chan int, 4)
	host, port := pop3Fixture(t, messages, retrieved, 1, 0)
	src := &POP3Source{host: host, port: port, username: "fixture-user", password: "fixture-password", useTLS: false, connTimeout: time.Second, readTimeout: time.Second}
	defer src.Close()
	scope := dedup.ComputeContentHash([]byte(fmt.Sprintf("pop3:%s:%d\x00%s", host, port, src.username)))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = plugins.WithKnownUIDs(ctx, func(gotScope, folder string) (map[string]bool, error) {
		if gotScope != scope || folder != "INBOX" {
			t.Fatalf("unexpected incremental lookup scope/folder: %q %q", gotScope, folder)
		}
		return map[string]bool{"older-id": true}, nil
	})
	since := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC).Unix()
	messagesOut, failures := src.FetchMessages(ctx, []string{"INBOX"}, since, 0)
	var got []*plugins.Message
	for messagesOut != nil || failures != nil {
		select {
		case msg, ok := <-messagesOut:
			if !ok {
				messagesOut = nil
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
	if len(got) != 1 || got[0].UID != "newer-id" || got[0].UIDScope != scope {
		t.Fatalf("unexpected messages: %+v", got)
	}
	if ids := []int{<-retrieved}; len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("RETR IDs = %v, want [1]", ids)
	}
	select {
	case id := <-retrieved:
		t.Fatalf("unexpected extra RETR %d", id)
	default:
	}
}

func TestPOP3FilteredFetchFailsOnInvalidDateHeader(t *testing.T) {
	host, port := pop3Fixture(t, []pop3FixtureMessage{{uid: "bad-date", raw: "Subject: missing date\r\n\r\nbody\r\n"}}, nil)
	src := &POP3Source{host: host, port: port, username: "fixture-user", password: "fixture-password", useTLS: false, connTimeout: time.Second, readTimeout: time.Second}
	defer src.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 1, 0)
	for range messages {
		t.Fatal("message without Date header passed a filtered fetch")
	}
	var failure error
	for err := range failures {
		failure = err
	}
	if failure == nil || !strings.Contains(failure.Error(), "valid Date header") {
		t.Fatalf("missing fail-closed Date error: %v", failure)
	}
}

func TestPOP3STARTTLSUpgradesBeforeAuthentication(t *testing.T) {
	certificate, roots := pop3TestCertificate(t)
	authenticated := make(chan struct{}, 1)
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		_, _ = fmt.Fprint(conn, "+OK fixture ready\r\n")
		reader := bufio.NewReader(conn)
		line, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(strings.ToUpper(line), "STLS") {
			return
		}
		_, _ = fmt.Fprint(conn, "+OK begin TLS\r\n")
		secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
		if secure.Handshake() != nil {
			return
		}
		scanner := bufio.NewScanner(secure)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			if len(parts) == 0 {
				return
			}
			switch strings.ToUpper(parts[0]) {
			case "USER", "PASS", "NOOP":
				select {
				case authenticated <- struct{}{}:
				default:
				}
				_, _ = fmt.Fprint(secure, "+OK accepted\r\n")
			case "QUIT":
				_, _ = fmt.Fprint(secure, "+OK bye\r\n")
				return
			default:
				_, _ = fmt.Fprint(secure, "-ERR unexpected command\r\n")
			}
		}
	})
	src := &POP3Source{host: host, port: port, username: "fixture-user", password: "fixture-password", useTLS: false, startTLS: true, connTimeout: time.Second, readTimeout: time.Second, tlsConfig: &tls.Config{RootCAs: roots}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := src.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-authenticated:
	default:
		t.Fatal("credentials were not sent after TLS upgrade")
	}
}

func TestPOP3XOAUTH2SendsExpectedPayload(t *testing.T) {
	verified := make(chan error, 1)
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		_, _ = fmt.Fprint(conn, "+OK fixture ready\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			if len(parts) == 3 && strings.EqualFold(parts[0], "AUTH") && strings.EqualFold(parts[1], "XOAUTH2") {
				payload, err := base64.StdEncoding.DecodeString(parts[2])
				if err == nil && string(payload) != "user=fixture-user\x01auth=Bearer fixture-access-token\x01\x01" {
					err = fmt.Errorf("unexpected XOAUTH2 payload %q", payload)
				}
				verified <- err
				if err != nil {
					_, _ = fmt.Fprint(conn, "-ERR invalid auth payload\r\n")
				} else {
					_, _ = fmt.Fprint(conn, "+OK authenticated\r\n")
				}
				continue
			}
			if len(parts) > 0 && strings.EqualFold(parts[0], "QUIT") {
				_, _ = fmt.Fprint(conn, "+OK bye\r\n")
				return
			}
			_, _ = fmt.Fprint(conn, "-ERR unexpected command\r\n")
		}
	})
	src := &POP3Source{host: host, port: port, username: "fixture-user", useTLS: false, connTimeout: time.Second, readTimeout: time.Second, tokenSource: func(context.Context) (string, error) { return "fixture-access-token", nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := src.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-verified; err != nil {
		t.Fatal(err)
	}
}

func TestPOP3XOAUTH2FailureRedactsEchoedPayload(t *testing.T) {
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		_, _ = fmt.Fprint(conn, "+OK fixture ready\r\n")
		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			return
		}
		parts := strings.Fields(scanner.Text())
		if len(parts) != 3 || !strings.EqualFold(parts[0], "AUTH") {
			return
		}
		_, _ = fmt.Fprintf(conn, "-ERR rejected %s\r\n", parts[2])
	})
	src := &POP3Source{host: host, port: port, username: "fixture-user", useTLS: false, connTimeout: time.Second, readTimeout: time.Second, tokenSource: func(context.Context) (string, error) { return "sensitive-fixture-token", nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := src.Connect(ctx)
	if err == nil || strings.Contains(err.Error(), "sensitive-fixture-token") || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte("user=fixture-user\x01auth=Bearer sensitive-fixture-token\x01\x01"))) {
		t.Fatalf("OAuth error leaked token or unexpectedly succeeded: %v", err)
	}
}

func pop3TestCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyPair := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey, Leaf: certificate}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return keyPair, roots
}
