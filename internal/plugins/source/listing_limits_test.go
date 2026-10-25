package source

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/zekihan/mailvault/internal/plugins"
)

func TestBoundedSearchUIDs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    imap.NumSet
		budget int
		want   []imap.UID
		fail   bool
	}{
		{"full uint32 range", imap.UIDSet{{Start: 1, Stop: math.MaxUint32}}, 10, nil, true},
		{"over budget", imap.UIDSet{{Start: 1, Stop: 11}}, 10, nil, true},
		{"cumulative budget", imap.UIDSet{{Start: 1, Stop: 6}, {Start: 10, Stop: 14}}, 10, nil, true},
		{"max endpoint", imap.UIDSet{{Start: math.MaxUint32 - 1, Stop: math.MaxUint32}}, 2, []imap.UID{math.MaxUint32 - 1, math.MaxUint32}, false},
		{"at budget", imap.UIDSet{{Start: 2, Stop: 4}}, 3, []imap.UID{2, 3, 4}, false},
		{"dynamic", imap.UIDSet{{Start: 1, Stop: 0}}, 10, nil, true},
		{"reversed", imap.UIDSet{{Start: 2, Stop: 1}}, 10, nil, true},
		{"sequence set", imap.SeqSet{{Start: 1, Stop: 1}}, 10, nil, true},
		{"empty", imap.UIDSet(nil), 10, []imap.UID{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := boundedSearchUIDs(&imap.SearchData{All: tc.set}, tc.budget)
			if (err != nil) != tc.fail || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("UIDs=%v err=%v, want %v fail=%v", got, err, tc.want, tc.fail)
			}
		})
	}
}

func TestListingConfiguration(t *testing.T) {
	for _, constructor := range []func(map[string]interface{}) (plugins.Source, error){NewIMapSource, NewPOP3Source} {
		for _, key := range []string{"max_listing_items", "max_listing_bytes"} {
			for _, value := range []interface{}{-1, "unlimited", 1.5, math.Inf(1)} {
				_, err := constructor(map[string]interface{}{"host": "example.invalid", "username": "fixture", "password_ref": "synthetic", key: value})
				if err == nil {
					t.Errorf("accepted %s=%v", key, value)
				}
			}
		}
	}
}

// The peer deliberately withholds a command completion/terminator after the
// hostile response. Rejection must close the socket rather than drain forever.
func listingFixture(t *testing.T, protocol, response string, count int) (string, int, *atomic.Bool, <-chan struct{}) {
	t.Helper()
	mutation := &atomic.Bool{}
	closed := make(chan struct{}, 8)
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		defer func() { closed <- struct{}{} }()
		if protocol == "imap" {
			fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1 ESEARCH] fixture\r\n")
		} else {
			fmt.Fprint(conn, "+OK fixture\r\n")
		}
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			if len(parts) == 0 {
				return
			}
			if protocol == "pop3" {
				switch parts[0] {
				case "USER", "PASS", "NOOP":
					fmt.Fprint(conn, "+OK\r\n")
				case "STAT":
					fmt.Fprintf(conn, "+OK %d 100\r\n", count)
				case "UIDL":
					fmt.Fprint(conn, response)
				case "RETR":
					mutation.Store(true)
					fmt.Fprint(conn, "-ERR fixture\r\n")
				case "QUIT":
					fmt.Fprint(conn, "+OK\r\n")
					return
				}
				continue
			}
			if len(parts) < 2 {
				return
			}
			tag := parts[0]
			switch parts[1] {
			case "LOGIN":
				fmt.Fprintf(conn, "%s OK login\r\n", tag)
			case "CAPABILITY":
				fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1 ESEARCH\r\n%s OK caps\r\n", tag)
			case "EXAMINE":
				fmt.Fprintf(conn, "* 0 EXISTS\r\n%s OK selected\r\n", tag)
			case "LIST":
				fmt.Fprint(conn, response)
			case "UID":
				if len(parts) > 2 && parts[2] == "SEARCH" {
					fmt.Fprintf(conn, "* ESEARCH (TAG \"%s\") UID ALL %s\r\n%s OK search\r\n", tag, response, tag)
				} else {
					mutation.Store(true)
				}
			case "CREATE", "APPEND":
				mutation.Store(true)
				fmt.Fprintf(conn, "%s BAD forbidden\r\n", tag)
			}
		}
	})
	return host, port, mutation, closed
}

func TestIMAPListBudgetsAbortDiscoveryAndRestore(t *testing.T) {
	for _, restore := range []bool{false, true} {
		for _, tc := range []struct {
			name, response string
			limits         listingLimits
		}{
			{"items", strings.Repeat("* LIST (\\Noselect) \"/\" \"Parent\"\r\n", 100), listingLimits{items: 2, bytes: 1 << 20}},
			{"quoted bytes", "* LIST () \"/\" \"" + strings.Repeat("x", 4096), listingLimits{items: 100, bytes: 128}},
			{"literal bytes", "* LIST () \"/\" {4294967295}\r\n" + strings.Repeat("x", 4096), listingLimits{items: 100, bytes: 128}},
		} {
			t.Run(fmt.Sprintf("%s/restore=%v", tc.name, restore), func(t *testing.T) {
				host, port, mutated, closed := listingFixture(t, "imap", tc.response, 0)
				src := &IMAPSource{host: host, port: port, username: "fixture", password: "synthetic", connTimeout: time.Second, readTimeout: time.Second, limits: tc.limits}
				defer src.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				var err error
				if restore {
					_, err = src.EnsureMailbox(ctx, "Restored", false)
				} else {
					_, err = src.ListFolders(ctx)
				}
				if !errors.Is(err, errListingLimit) {
					t.Fatalf("expected listing limit, got %v", err)
				}
				if restore && !errors.Is(err, plugins.ErrMailboxUnchanged) {
					t.Fatalf("restore lost unchanged marker: %v", err)
				}
				if mutated.Load() {
					t.Fatal("mailbox mutation after incomplete listing")
				}
				select {
				case <-closed:
				case <-ctx.Done():
					t.Fatal("server connection was not closed")
				}
				if src.connected {
					t.Fatal("failed session remained connected")
				}
			})
		}
	}
}

func TestIMAPSearchRejectsHostileRangeOnWire(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		maxBytes       int
	}{
		{"full uint32 range", "1:4294967295", 4096},
		{"UID budget", "1:11", 4096},
		{"byte budget", strings.Repeat("1,", 1000) + "2", 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port, mutated, _ := listingFixture(t, "imap", tc.response, 0)
			src := &IMAPSource{host: host, port: port, username: "fixture", password: "synthetic", connTimeout: time.Second, readTimeout: time.Second, limits: listingLimits{items: 10, bytes: tc.maxBytes}}
			defer src.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
			for range messages {
				t.Error("message emitted after oversized search")
			}
			var err error
			for failure := range failures {
				err = failure
			}
			if !errors.Is(err, errListingLimit) {
				t.Fatalf("expected limit, got %v", err)
			}
			if mutated.Load() {
				t.Fatal("FETCH after oversized search")
			}
		})
	}
}

func TestPOP3ListingBudgets(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		count          int
		limits         listingLimits
		want           string
	}{
		{"negative STAT", "-ERR UIDL unsupported\r\n", -1, listingLimits{items: 10, bytes: 1024}, "STAT count"},
		{"huge STAT", "-ERR UIDL unsupported\r\n", math.MaxInt, listingLimits{items: 10, bytes: 1024}, "STAT count"},
		{"UIDL items", "+OK\r\n1 one\r\n2 two\r\n3 three\r\n", 2, listingLimits{items: 2, bytes: 1024}, "item budget"},
		{"UIDL bytes", "+OK\r\n1 " + strings.Repeat("x", 4096), 2, listingLimits{items: 2, bytes: 64}, "byte budget"},
		{"status bytes", "+OK " + strings.Repeat("x", 4096), 2, listingLimits{items: 2, bytes: 64}, "byte budget"},
		{"duplicate UID", "+OK\r\n1 same\r\n2 same\r\n", 2, listingLimits{items: 2, bytes: 1024}, "duplicate UIDL"},
		{"invalid ID", "+OK\r\n0 bad\r\n", 2, listingLimits{items: 2, bytes: 1024}, "invalid or duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port, retrieved, closed := listingFixture(t, "pop3", tc.response, tc.count)
			src := &POP3Source{host: host, port: port, username: "fixture", password: "synthetic", connTimeout: time.Second, readTimeout: time.Second, limits: tc.limits}
			defer src.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
			for range messages {
				t.Error("message emitted after invalid listing")
			}
			var err error
			for failure := range failures {
				err = failure
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if retrieved.Load() {
				t.Fatal("RETR after invalid listing")
			}
			select {
			case <-closed:
			case <-ctx.Done():
				t.Fatal("server connection was not closed")
			}
		})
	}
}

func TestPOP3BoundedUIDLAtLimitRetainsProtocolState(t *testing.T) {
	const count = 1000
	messages := make([]pop3FixtureMessage, count)
	known := make(map[string]bool, count)
	responseBytes := len("+OK identifiers\r\n.\r\n")
	for i := range messages {
		uid := fmt.Sprintf("fixture-%d", i+1)
		messages[i] = pop3FixtureMessage{uid: uid}
		known[uid] = true
		responseBytes += len(fmt.Sprintf("%d %s\r\n", i+1, uid))
	}
	host, port := pop3Fixture(t, messages, nil)
	src := &POP3Source{host: host, port: port, username: "fixture", password: "synthetic", connTimeout: time.Second, readTimeout: time.Second, limits: listingLimits{items: count, bytes: responseBytes}}
	defer src.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = plugins.WithKnownUIDs(ctx, func(string, string) (map[string]bool, error) { return known, nil })
	out, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	for range out {
		t.Error("known message was fetched")
	}
	for err := range failures {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("QUIT after streamed UIDL: %v", err)
	}
}

func TestPOP3UIDLUnsupportedUsesLazySequenceIdentities(t *testing.T) {
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		fmt.Fprint(conn, "+OK fixture\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			if len(parts) == 0 {
				return
			}
			switch parts[0] {
			case "USER", "PASS", "NOOP", "QUIT":
				fmt.Fprint(conn, "+OK\r\n")
			case "STAT":
				fmt.Fprint(conn, "+OK 2 100\r\n")
			case "UIDL":
				fmt.Fprint(conn, "-ERR UIDL unsupported\r\n")
			case "LIST":
				raw := fmt.Sprintf("Subject: synthetic %s\r\n\r\nbody\r\n", parts[1])
				fmt.Fprintf(conn, "+OK %s %d\r\n", parts[1], len(raw))
			case "RETR":
				fmt.Fprintf(conn, "+OK\r\nSubject: synthetic %s\r\n\r\nbody\r\n.\r\n", parts[1])
			}
		}
	})
	src := &POP3Source{host: host, port: port, username: "fixture", password: "synthetic", connTimeout: time.Second, readTimeout: time.Second, limits: listingLimits{items: 2, bytes: 1024}}
	defer src.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	var uids []string
	for msg := range out {
		if msg.UIDScope != "" {
			t.Errorf("unstable identity has scope %q", msg.UIDScope)
		}
		uids = append(uids, msg.UID)
	}
	for err := range failures {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(uids, []string{"sequence:1", "sequence:2"}) {
		t.Fatalf("fallback UIDs=%v", uids)
	}
}

func TestIMAPListAtLimitPreservesDecodedMailboxNames(t *testing.T) {
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] fixture\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			if len(parts) < 2 {
				return
			}
			tag := parts[0]
			switch parts[1] {
			case "LOGIN":
				fmt.Fprintf(conn, "%s OK login\r\n", tag)
			case "CAPABILITY":
				fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK caps\r\n", tag)
			case "LIST":
				fmt.Fprintf(conn, "* LIST (\\Noselect) \"/\" \"Parent\"\r\n* LIST () \"/\" \"INBOX\"\r\n* LIST () \"/\" \"&ZeVnLIqe-\"\r\n%s OK listed\r\n", tag)
			}
		}
	})
	src := &IMAPSource{host: host, port: port, username: "fixture", password: "synthetic", connTimeout: time.Second, readTimeout: time.Second, limits: listingLimits{items: 3, bytes: 4096}}
	defer src.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	folders, err := src.ListFolders(ctx)
	if err != nil || !reflect.DeepEqual(folders, []string{"INBOX", "日本語"}) {
		t.Fatalf("folders=%v err=%v", folders, err)
	}
	if _, err := src.EnsureMailbox(ctx, "日本語", true); err != nil {
		t.Fatalf("restore check after streamed LIST: %v", err)
	}
}

func TestPOP3TLSListingBudgetDoesNotLimitMessageBody(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("starttls=%v", startTLS), func(t *testing.T) {
			certificate, roots := pop3TestCertificate(t)
			raw := "Subject: synthetic\r\n\r\n" + strings.Repeat("x", 10000) + "\r\n"
			host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
				if startTLS {
					fmt.Fprint(conn, "+OK fixture\r\n")
					line, err := bufio.NewReader(conn).ReadString('\n')
					if err != nil || strings.TrimSpace(line) != "STLS" {
						return
					}
					fmt.Fprint(conn, "+OK start TLS\r\n")
				}
				secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
				if secure.Handshake() != nil {
					return
				}
				if !startTLS {
					fmt.Fprint(secure, "+OK fixture\r\n")
				}
				scanner := bufio.NewScanner(secure)
				for scanner.Scan() {
					parts := strings.Fields(scanner.Text())
					if len(parts) == 0 {
						return
					}
					switch parts[0] {
					case "USER", "PASS", "NOOP":
						fmt.Fprint(secure, "+OK\r\n")
					case "STAT":
						fmt.Fprint(secure, "+OK 1 10000\r\n")
					case "UIDL":
						fmt.Fprint(secure, "+OK\r\n1 fixture-id\r\n.\r\n")
					case "LIST":
						fmt.Fprintf(secure, "+OK %s %d\r\n", parts[1], len(raw))
					case "RETR":
						fmt.Fprintf(secure, "+OK\r\n%s.\r\n", raw)
					case "QUIT":
						fmt.Fprint(secure, "+OK\r\n")
						return
					}
				}
			})
			src := &POP3Source{host: host, port: port, username: "fixture", password: "synthetic", useTLS: !startTLS, startTLS: startTLS, tlsConfig: &tls.Config{RootCAs: roots}, connTimeout: time.Second, readTimeout: time.Second, limits: listingLimits{items: 1, bytes: 128}}
			defer src.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
			count := 0
			for msg := range out {
				count++
				if string(msg.Raw) != raw {
					t.Errorf("message body changed: got %d bytes, want %d", len(msg.Raw), len(raw))
				}
			}
			for err := range failures {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("messages=%d", count)
			}
		})
	}
}
