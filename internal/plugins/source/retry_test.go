package source

import (
	"context"
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/zekihan/mailvault/internal/plugins"
)

func TestExcludeDeliveredPreservesHoles(t *testing.T) {
	c := imap.SearchCriteria{}
	excludeDelivered(&c, map[string]bool{"1": true, "2": true, "4": true, "3": false, "0": true, "bad": true})
	if len(c.Not) != 1 || c.Not[0].UID[0].String() != "1:2,4" {
		t.Fatalf("unsafe search: %+v", c)
	}
}

func TestRetryFetchStableIdentity(t *testing.T) {
	out := make(chan *plugins.Message, 4)
	attempts := 0
	err := retryFetch(context.Background(), out, func(ctx context.Context, ch chan<- *plugins.Message) error {
		attempts++
		ch <- &plugins.Message{UIDScope: "generation", Folder: "INBOX", UID: "1"}
		if attempts == 1 {
			return io.ErrUnexpectedEOF
		}
		ch <- &plugins.Message{UIDScope: "generation", Folder: "INBOX", UID: "2"}
		return nil
	})
	if err != nil || attempts != 2 || len(out) != 2 {
		t.Fatalf("err=%v attempts=%d messages=%d", err, attempts, len(out))
	}
}

func TestRetryFetchDoesNotReplayUnstableIdentity(t *testing.T) {
	out := make(chan *plugins.Message, 4)
	attempts := 0
	err := retryFetch(context.Background(), out, func(ctx context.Context, ch chan<- *plugins.Message) error {
		attempts++
		ch <- &plugins.Message{UID: "sequence:1"}
		return io.EOF
	})
	if !errors.Is(err, io.EOF) || attempts != 1 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}

func TestServerRejectionIsNotMarkedAsTransportFailure(t *testing.T) {
	err := joinTransportError(errors.New("IMAP server rejected UID FETCH"), &deadlineConn{})
	if transientSourceError(err) {
		t.Fatalf("server rejection classified as transient transport failure: %v", err)
	}
}

func TestRetryFetchCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := retryFetch(ctx, make(chan *plugins.Message), func(ctx context.Context, ch chan<- *plugins.Message) error {
		attempts++
		cancel()
		return io.EOF
	})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}

func TestRetryFetchPublishesKnownUIDs(t *testing.T) {
	attempts := 0
	out := make(chan *plugins.Message, 2)
	err := retryFetch(context.Background(), out, func(ctx context.Context, ch chan<- *plugins.Message) error {
		attempts++
		known, err := plugins.KnownUIDs(ctx, "scope", "INBOX")
		if err != nil {
			return err
		}
		if attempts == 2 && !known["42"] {
			return errors.New("retry lost streamed identity")
		}
		if !known["42"] {
			ch <- &plugins.Message{UIDScope: "scope", Folder: "INBOX", UID: "42"}
		}
		if attempts == 1 {
			return io.EOF
		}
		return nil
	})
	if err != nil || attempts != 2 || len(out) != 1 {
		t.Fatalf("err=%v attempts=%d messages=%d", err, attempts, len(out))
	}
}

func TestExcludeDeliveredBoundsCommandSize(t *testing.T) {
	known := make(map[string]bool)
	for i := 1; i < 20000; i += 2 {
		known[strconv.Itoa(i)] = true
	}
	criteria := imap.SearchCriteria{}
	excludeDelivered(&criteria, known)
	if len(criteria.Not) != 0 {
		t.Fatal("oversized exclusion command")
	}
}
