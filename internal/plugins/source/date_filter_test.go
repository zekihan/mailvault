package source

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
)

func datedIMAPSource(t *testing.T, dates []string) (*IMAPSource, <-chan string) {
	t.Helper()
	searches := make(chan string, 1)
	host, port := protocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			line := scanner.Text()
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return
			}
			tag := parts[0]
			switch parts[1] {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
			case "CAPABILITY":
				_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", tag)
			case "EXAMINE":
				_, _ = fmt.Fprintf(conn, "* %d EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n* OK [UIDNEXT %d] next\r\n* OK [HIGHESTMODSEQ 99] modified\r\n%s OK [READ-ONLY] selected\r\n", len(dates), len(dates)+1, tag)
			case "UID":
				if parts[2] == "SEARCH" {
					searches <- line
					_, _ = fmt.Fprint(conn, "* SEARCH")
					for i := range dates {
						_, _ = fmt.Fprintf(conn, " %d", i+1)
					}
					_, _ = fmt.Fprintf(conn, "\r\n%s OK searched\r\n", tag)
				} else {
					for i, date := range dates {
						dateItem := ""
						if date != "" {
							dateItem = fmt.Sprintf("INTERNALDATE %q ", date)
						}
						raw := fmt.Sprintf("Message-ID: <fixture-%d@example.invalid>\r\n\r\nbody-%d\r\n", i+1, i+1)
						_, _ = fmt.Fprintf(conn, "* %d FETCH (UID %d %sBODY[] {%d}\r\n%s)\r\n", i+1, i+1, dateItem, len(raw), raw)
					}
					_, _ = fmt.Fprintf(conn, "%s OK fetched\r\n", tag)
				}
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected command\r\n", tag)
			}
		}
	})
	src := &IMAPSource{host: host, port: port, username: "test-user", password: "synthetic-value", connTimeout: time.Second, readTimeout: time.Second, maxConns: 100}
	t.Cleanup(func() { _ = src.Close() })
	return src, searches
}

func TestIMAPReportsSelectedFolderState(t *testing.T) {
	src, _ := datedIMAPSource(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var observation plugins.FolderStateObservation
	ctx = plugins.WithFolderStateObserver(ctx, func(value plugins.FolderStateObservation) error {
		observation = value
		return nil
	})
	messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	for messages != nil || failures != nil {
		select {
		case _, ok := <-messages:
			if !ok {
				messages = nil
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
	if observation != (plugins.FolderStateObservation{Folder: "INBOX", UIDValidity: "1", UIDNext: "1", ModSeq: "99"}) {
		t.Fatalf("folder observation = %+v", observation)
	}
}

func fetchDatedMessages(t *testing.T, src *IMAPSource, since, until int64) ([]*plugins.Message, []error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	messages, failures := src.FetchMessages(ctx, []string{"INBOX"}, since, until)
	var result []*plugins.Message
	var errs []error
	for messages != nil || failures != nil {
		select {
		case msg, ok := <-messages:
			if !ok {
				messages = nil
			} else {
				result = append(result, msg)
			}
		case err, ok := <-failures:
			if !ok {
				failures = nil
			} else if err != nil {
				errs = append(errs, err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	return result, errs
}

func TestIMAPExactTimestampBounds(t *testing.T) {
	dates := []string{"01-Jan-2024 00:00:00 +0000", "01-Jan-2024 12:00:00 +0000", "01-Jan-2024 15:00:00 +0000", "02-Jan-2024 10:00:00 +0000", "02-Jan-2024 00:30:00 +1400", "01-Jan-2024 23:00:00 -1200", "03-Jan-2024 01:30:00 +1400", "02-Jan-2024 12:00:00 +0000", "02-Jan-2024 13:00:00 +0000"}
	since := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC).Unix()
	until := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC).Unix()
	for _, tc := range []struct {
		name         string
		lower, upper int64
		want         string
	}{{"interval", since, until, "2,3,4,6,7"}, {"since-only", since, 0, "2,3,4,6,7,8,9"}, {"until-only", 0, until, "1,2,3,4,5,6,7"}, {"unfiltered", 0, 0, "1,2,3,4,5,6,7,8,9"}} {
		t.Run(tc.name, func(t *testing.T) {
			src, searches := datedIMAPSource(t, dates)
			messages, errs := fetchDatedMessages(t, src, tc.lower, tc.upper)
			if len(errs) > 0 {
				t.Fatal(errs)
			}
			var ids []string
			for _, msg := range messages {
				ids = append(ids, msg.UID)
			}
			sort.Strings(ids)
			if got := strings.Join(ids, ","); got != tc.want {
				t.Fatalf("fetched UIDs %q, want %q", got, tc.want)
			}
			command := <-searches
			if tc.lower != 0 && !strings.Contains(command, "31-Dec-2023") {
				t.Fatalf("search could exclude timezone-shifted lower-bound messages: %s", command)
			}
			if tc.upper != 0 && !strings.Contains(command, "4-Jan-2024") {
				t.Fatalf("search could exclude timezone-shifted upper-bound messages: %s", command)
			}
		})
	}
}

func TestIMAPFilteringRequiresInternalDate(t *testing.T) {
	src, _ := datedIMAPSource(t, []string{""})
	messages, errs := fetchDatedMessages(t, src, 1, 0)
	if len(messages) != 0 || len(errs) == 0 || !strings.Contains(errs[0].Error(), "INTERNALDATE") {
		t.Fatalf("missing date did not fail closed: messages=%d errors=%v", len(messages), errs)
	}
}
