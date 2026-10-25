package source

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/zekihan/mailvault/internal/plugins"
)

func transientSourceError(err error) bool {
	if errors.Is(err, errListingLimit) {
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

// Retry transport failures without re-emitting stable identities already streamed.
func retryFetch(ctx context.Context, out chan<- *plugins.Message, fetch func(context.Context, chan<- *plugins.Message) error) error {
	type identity struct{ scope, folder, uid string }
	emitted := make(map[identity]bool)
	unstable := false
	for attempt := 0; ; attempt++ {
		messages := make(chan *plugins.Message)
		result := make(chan error, 1)
		snapshot := make(map[identity]bool, len(emitted))
		for key := range emitted {
			snapshot[key] = true
		}
		attemptCtx := plugins.WithKnownUIDs(ctx, func(scope, folder string) (map[string]bool, error) {
			known, err := plugins.KnownUIDs(ctx, scope, folder)
			if err != nil {
				return nil, err
			}
			merged := make(map[string]bool, len(known))
			for uid, delivered := range known {
				merged[uid] = delivered
			}
			for key := range snapshot {
				if key.scope == scope && key.folder == folder {
					merged[key.uid] = true
				}
			}
			return merged, nil
		})
		go func() { result <- fetch(attemptCtx, messages); close(messages) }()
		for msg := range messages {
			key := identity{msg.UIDScope, msg.Folder, msg.UID}
			if msg.UIDScope != "" && emitted[key] {
				continue
			}
			select {
			case out <- msg:
				if msg.UIDScope == "" {
					unstable = true
				} else {
					emitted[key] = true
				}
			case <-ctx.Done():
				// Drain the producer before returning so its connection lock stays owned.
			}
		}
		err := <-result
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil || unstable || attempt >= 2 || !transientSourceError(err) {
			return err
		}
		plugins.ReportFetchProgress(ctx, plugins.FetchProgress{Total: -1, Stage: "connection lost; retrying"})
		timer := time.NewTimer(time.Duration(1<<attempt) * 200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
