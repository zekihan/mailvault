package source

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestIMAPXOAUTH2SendsExpectedPayload(t *testing.T) {
	verified := make(chan error, 1)
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		_, _ = fmt.Fprint(conn, "* OK fixture ready\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			line := scanner.Text()
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return
			}
			tag := parts[0]
			switch strings.ToUpper(parts[1]) {
			case "CAPABILITY":
				_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1 SASL-IR AUTH=XOAUTH2\r\n%s OK capabilities\r\n", tag)
			case "AUTHENTICATE":
				if len(parts) != 4 || !strings.EqualFold(parts[2], "XOAUTH2") {
					verified <- fmt.Errorf("unexpected AUTHENTICATE command %q", line)
					_, _ = fmt.Fprintf(conn, "%s NO invalid command\r\n", tag)
					continue
				}
				payload, err := base64.StdEncoding.DecodeString(parts[3])
				if err == nil && string(payload) != "user=fixture-user\x01auth=Bearer fixture-access-token\x01\x01" {
					err = fmt.Errorf("unexpected XOAUTH2 payload %q", payload)
				}
				verified <- err
				if err != nil {
					_, _ = fmt.Fprintf(conn, "%s NO invalid auth payload\r\n", tag)
				} else {
					_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
				}
			case "LOGOUT":
				_, _ = fmt.Fprintf(conn, "* BYE closing\r\n%s OK logout\r\n", tag)
				return
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
			}
		}
	})
	src := &IMAPSource{host: host, port: port, username: "fixture-user", useTLS: false, connTimeout: time.Second, readTimeout: time.Second, tokenSource: func(context.Context) (string, error) { return "fixture-access-token", nil }}
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

func TestIMAPXOAUTH2FailureRedactsEchoedPayload(t *testing.T) {
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		_, _ = fmt.Fprint(conn, "* OK fixture ready\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			if len(parts) < 2 {
				return
			}
			tag := parts[0]
			switch strings.ToUpper(parts[1]) {
			case "CAPABILITY":
				_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1 SASL-IR AUTH=XOAUTH2\r\n%s OK capabilities\r\n", tag)
			case "AUTHENTICATE":
				_, _ = fmt.Fprintf(conn, "%s NO rejected %s\r\n", tag, parts[3])
				return
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
			}
		}
	})
	src := &IMAPSource{host: host, port: port, username: "fixture-user", useTLS: false, connTimeout: time.Second, readTimeout: time.Second, tokenSource: func(context.Context) (string, error) { return "sensitive-fixture-token", nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := src.Connect(ctx)
	if err == nil || strings.Contains(err.Error(), "sensitive-fixture-token") || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte("user=fixture-user\x01auth=Bearer sensitive-fixture-token\x01\x01"))) {
		t.Fatalf("OAuth error leaked token or unexpectedly succeeded: %v", err)
	}
}
