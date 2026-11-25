package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/zekihan/mailvault/internal/plugins"
)

func transientSourceError(err error) bool {
	if errors.Is(err, plugins.ErrMessageTooLarge) || errors.Is(err, errListingLimit) || errors.Is(err, errConnectRetriesExhausted) {
		return false
	}
	var transport net.Error
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &transport)
}

func joinTransportError(err error, wire *deadlineConn) error {
	var statusErr *imap.Error
	if errors.As(err, &statusErr) && (statusErr.Type == imap.StatusResponseTypeNo || statusErr.Type == imap.StatusResponseTypeBad) {
		return err
	}
	if transportErr := wire.transportError(); transportErr != nil {
		return errors.Join(err, transportErr)
	}
	return err
}

type streamedUID struct{ scope, folder, uid string }
type streamedUIDsKey struct{}

// Include streamed UIDs when selecting bodies for a retry. They are separate
// from delivery receipts because the consumer may have failed to store them.
func skipStreamedUIDs(ctx context.Context, scope, folder string, known map[string]bool) map[string]bool {
	var streamed map[streamedUID]bool
	if v, ok := ctx.Value(streamedUIDsKey{}).(map[streamedUID]bool); ok {
		streamed = v
	}
	var merged map[string]bool
	for key := range streamed {
		if key.scope != scope || key.folder != folder {
			continue
		}
		if merged == nil {
			merged = make(map[string]bool, len(known))
			maps.Copy(merged, known)
		}
		merged[key.uid] = true
	}
	if merged == nil {
		return known
	}
	return merged
}

// Retry transport failures without re-emitting stable identities already streamed.
func retryFetch(ctx context.Context, out chan<- *plugins.Message, fetch func(context.Context, chan<- *plugins.Message) error) error {
	emitted := make(map[streamedUID]bool)
	unstable := false
	for attempt := 0; ; attempt++ {
		err := runFetchAttempt(ctx, out, fetch, emitted, &unstable)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil || unstable || attempt >= 2 || !transientSourceError(err) {
			return err
		}
		if err := waitRetryBackoff(ctx, attempt); err != nil {
			return err
		}
	}
}

func runFetchAttempt(ctx context.Context, out chan<- *plugins.Message, fetch func(context.Context, chan<- *plugins.Message) error, emitted map[streamedUID]bool, unstable *bool) error {
	messages := make(chan *plugins.Message)
	result := make(chan error, 1)
	attemptCtx := context.WithValue(ctx, streamedUIDsKey{}, maps.Clone(emitted))
	go func() { result <- fetch(attemptCtx, messages); close(messages) }()
	for msg := range messages {
		forwardFetchedMessage(ctx, out, msg, emitted, unstable)
	}
	return <-result
}

func forwardFetchedMessage(ctx context.Context, out chan<- *plugins.Message, msg *plugins.Message, emitted map[streamedUID]bool, unstable *bool) {
	key := streamedUID{msg.UIDScope, msg.Folder, msg.UID}
	if msg.UIDScope != "" && emitted[key] {
		return
	}
	select {
	case out <- msg:
		if msg.UIDScope == "" {
			*unstable = true
		} else {
			emitted[key] = true
		}
	case <-ctx.Done():
		// Drain the producer before returning so its connection lock stays owned.
	}
}

func waitRetryBackoff(ctx context.Context, attempt int) error {
	plugins.ReportFetchProgress(ctx, plugins.FetchProgress{Total: -1, Stage: "connection lost; retrying"})
	timer := time.NewTimer(time.Duration(1<<attempt) * 200 * time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Initial connection backoff: 1s, 2s, 4s, and so on, capped at 30s until the configured budget would be exceeded by the next wait.
const (
	DefaultConnectRetryTimeout = 3 * time.Minute
	connectRetryInitialDelay   = time.Second
	connectRetryMaxDelay       = 30 * time.Second
)

// errPermanentConnect marks connection failures that retrying cannot fix,
// such as credential rejections.
var errPermanentConnect = errors.New("permanent connection failure")

// errConnectRetriesExhausted stops the fetch-level retry from multiplying an
// already spent connection budget.
var errConnectRetriesExhausted = errors.New("connection retries exhausted")

func permanentConnectError(err error) error { return errors.Join(err, errPermanentConnect) }

type connectBackoff struct {
	budget  time.Duration
	initial time.Duration
	max     time.Duration
}

func retryableConnectError(err error) bool {
	return !errors.Is(err, errPermanentConnect) && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) && (transientSourceError(err) || temporaryIMAPFailure(err))
}

// temporaryIMAPFailure reports NO [UNAVAILABLE] (RFC 5530), e.g. a backend
// that accepts connections before it can serve logins.
func temporaryIMAPFailure(err error) bool {
	var statusErr *imap.Error
	return errors.As(err, &statusErr) && statusErr.Code == imap.ResponseCodeUnavailable
}

// retryConnect retries transient dial, handshake, greeting and login transport
// failures with bounded exponential backoff. A zero budget makes one attempt.
func retryConnect(ctx context.Context, policy connectBackoff, name string, connect func(context.Context) error) error {
	if policy.initial <= 0 {
		policy.initial = connectRetryInitialDelay
	}
	if policy.max <= 0 {
		policy.max = connectRetryMaxDelay
	}
	deadline := time.Now().Add(policy.budget)
	delay := policy.initial
	for attempt := 1; ; attempt++ {
		err := connect(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if policy.budget <= 0 || !retryableConnectError(err) {
			return err
		}
		if time.Now().Add(delay).After(deadline) {
			return fmt.Errorf("%w after %d attempts within %s: %w", errConnectRetriesExhausted, attempt, policy.budget, err)
		}
		slog.WarnContext(ctx, "Source connection failed; retrying",
			slog.String("source", name),
			slog.Int("attempt", attempt),
			slog.String("retryIn", delay.String()),
			slog.Any("error", err))
		plugins.ReportFetchProgress(ctx, plugins.FetchProgress{Total: -1, Stage: fmt.Sprintf("connection attempt %d failed; retrying in %s", attempt, delay)})
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, policy.max)
	}
}
