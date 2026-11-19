package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/zekihan/mailvault/internal/plugins"
)

func TestAppendFlagsPreserveSafeFlagsAndExcludeRecent(t *testing.T) {
	flags, err := appendFlags([]string{"\\Seen", "\\Recent", "$Label1", "Project-Blue"})
	if err != nil {
		t.Fatal(err)
	}
	want := []imap.Flag{imap.FlagSeen, "$Label1", "Project-Blue"}
	if !reflect.DeepEqual(flags, want) {
		t.Fatalf("flags = %#v, want %#v", flags, want)
	}
}

func TestEnsureMailboxDistinguishesCreateConflictFromDisconnect(t *testing.T) {
	for _, tt := range []struct {
		name     string
		response string
		conflict bool
	}{
		{name: "already exists", response: "NO [ALREADYEXISTS] Mailbox already exists", conflict: true},
		{name: "connection lost"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var createReceived atomic.Bool
			host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
				_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] fixture ready\r\n")
				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					parts := strings.Fields(scanner.Text())
					if len(parts) < 2 {
						return
					}
					tag := parts[0]
					switch parts[1] {
					case "LOGIN":
						_, _ = fmt.Fprintf(conn, "%s OK logged in\r\n", tag)
					case "CAPABILITY":
						_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", tag)
					case "LIST":
						_, _ = fmt.Fprintf(conn, "%s OK listed\r\n", tag)
					case "CREATE":
						createReceived.Store(true)
						if tt.response == "" {
							return
						}
						_, _ = fmt.Fprintf(conn, "%s %s\r\n", tag, tt.response)
					default:
						_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
					}
				}
			})
			source := &IMAPSource{host: host, port: port, username: "test-user", password: "test-value", connTimeout: time.Second, readTimeout: time.Second}
			defer source.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			created, err := source.EnsureMailbox(ctx, "Restored", false)
			if !createReceived.Load() || created || err == nil {
				t.Fatalf("expected a failed CREATE, got received=%t created=%t err=%v", createReceived.Load(), created, err)
			}
			if errors.Is(err, plugins.ErrMailboxExists) != tt.conflict || errors.Is(err, plugins.ErrMailboxUnchanged) {
				t.Fatalf("CREATE conflict classification, want conflict=%t, got %v", tt.conflict, err)
			}
		})
	}
}

func TestAppendRejectsRecreatedMailboxGenerationOnWire(t *testing.T) {
	var appendReceived atomic.Bool
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
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK logged in\r\n", tag)
			case "CAPABILITY":
				_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capability\r\n", tag)
			case "LIST":
				_, _ = fmt.Fprintf(conn, "* LIST () \"/\" \"Restored\"\r\n%s OK listed\r\n", tag)
			case "EXAMINE", "SELECT":
				_, _ = fmt.Fprintf(conn, "* 0 EXISTS\r\n* OK [UIDVALIDITY 8] generation\r\n%s OK [READ-ONLY] selected\r\n", tag)
			case "APPEND":
				appendReceived.Store(true)
				return
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
			}
		}
	})
	source := &IMAPSource{host: host, port: port, username: "test-user", password: "test-value", connTimeout: time.Second, readTimeout: time.Second}
	defer source.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	created, err := source.EnsureMailbox(ctx, "Restored", true)
	if err != nil || created {
		t.Fatalf("ensure existing mailbox: created=%t err=%v", created, err)
	}
	validity, err := source.MailboxUIDValidity(ctx, "Restored")
	if err != nil || validity != "8" {
		t.Fatalf("recreated mailbox generation = %q, err = %v", validity, err)
	}
	if err := source.AppendMessage(ctx, "Restored", &plugins.Message{Raw: []byte("message")}, "7"); err == nil || !strings.Contains(err.Error(), "UIDVALIDITY changed from 7 to 8 before APPEND") {
		t.Fatalf("expected generation rejection before APPEND, got %v", err)
	}
	if appendReceived.Load() {
		t.Fatal("APPEND was sent to the recreated mailbox")
	}
}

func TestAppendRejectsGenerationChangeAfterUpload(t *testing.T) {
	for _, mode := range []string{"appenduid", "postselect"} {
		t.Run(mode, func(t *testing.T) {
			var appendReceived atomic.Bool
			host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
				_, _ = fmt.Fprint(conn, "* OK fixture ready\r\n")
				reader := bufio.NewReader(conn)
				selectCount := 0
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					parts := strings.Fields(line)
					if len(parts) < 2 {
						return
					}
					tag := parts[0]
					switch strings.ToUpper(parts[1]) {
					case "LOGIN":
						_, _ = fmt.Fprintf(conn, "%s OK logged in\r\n", tag)
					case "CAPABILITY":
						_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capability\r\n", tag)
					case "EXAMINE", "SELECT":
						selectCount++
						validity := 7
						if mode == "postselect" && selectCount > 1 {
							validity = 8
						}
						_, _ = fmt.Fprintf(conn, "* 0 EXISTS\r\n* OK [UIDVALIDITY %d] generation\r\n%s OK [READ-ONLY] selected\r\n", validity, tag)
					case "APPEND":
						appendReceived.Store(true)
						open := strings.LastIndex(line, "{")
						if open < 0 {
							return
						}
						close := strings.Index(line[open:], "}")
						if close < 0 {
							return
						}
						size, err := strconv.Atoi(line[open+1 : open+close])
						if err != nil {
							return
						}
						_, _ = fmt.Fprint(conn, "+ Ready for literal\r\n")
						literal := make([]byte, size+2)
						if _, err := io.ReadFull(reader, literal); err != nil {
							return
						}
						if mode == "appenduid" {
							_, _ = fmt.Fprintf(conn, "%s OK [APPENDUID 8 1] appended\r\n", tag)
						} else {
							_, _ = fmt.Fprintf(conn, "%s OK appended\r\n", tag)
						}
					default:
						_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", tag)
					}
				}
			})
			source := &IMAPSource{host: host, port: port, username: "test-user", password: "test-value", connTimeout: time.Second, readTimeout: time.Second}
			defer source.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := source.AppendMessage(ctx, "Restored", &plugins.Message{Raw: []byte("message")}, "7")
			if err == nil || !strings.Contains(err.Error(), "UIDVALIDITY changed during APPEND from 7 to 8") {
				t.Fatalf("expected generation rejection after APPEND, got %v", err)
			}
			if !appendReceived.Load() {
				t.Fatal("fixture did not receive APPEND")
			}
		})
	}
}

func TestAppendFlagsRejectUnsafeAndUnknownSystemFlags(t *testing.T) {
	for _, flag := range []string{"\\Unknown", "bad keyword", "x) LOGIN"} {
		if _, err := appendFlags([]string{flag}); err == nil {
			t.Errorf("accepted unsafe flag %q", flag)
		}
	}
}
