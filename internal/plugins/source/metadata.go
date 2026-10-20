package source

import (
	"context"
	"errors"
	"strconv"

	"github.com/emersion/go-imap/v2"
	"github.com/zekihan/mailvault/internal/plugins"
)

func (s *IMAPSource) refreshMetadata(ctx context.Context, session *imapSession, scope, folder string, known map[string]bool) error {
	observe := plugins.MetadataObserver(ctx)
	if observe == nil || len(known) == 0 {
		return nil
	}
	ids := make([]imap.UID, 0, len(known))
	for value, delivered := range known {
		id, err := strconv.ParseUint(value, 10, 32)
		if err == nil && id != 0 && delivered {
			ids = append(ids, imap.UID(id))
		}
	}
	for start := 0; start < len(ids); start += 100 {
		end := start + 100
		if end > len(ids) {
			end = len(ids)
		}
		finish := session.wire.beginIO(ctx)
		fetch := session.client.Fetch(imap.UIDSetNum(ids[start:end]...), &imap.FetchOptions{Flags: true})
		var failure error
		for item := fetch.Next(); item != nil; item = fetch.Next() {
			data, err := item.Collect()
			if err != nil {
				failure = err
				break
			}
			msg := &plugins.Message{UIDScope: scope, Folder: folder, UID: strconv.FormatUint(uint64(data.UID), 10)}
			for _, flag := range data.Flags {
				msg.Flags = append(msg.Flags, string(flag))
			}
			if err := observe(msg); err != nil {
				failure = err
				break
			}
		}
		failure = errors.Join(failure, fetch.Close())
		finish()
		if failure != nil {
			return joinTransportError(failure, session.wire)
		}
	}
	return nil
}
