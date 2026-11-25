package source

import (
	"errors"
	"fmt"

	"github.com/emersion/go-imap/v2"
)

// listMailboxesLocked streams even non-selectable entries through the same
// budget for discovery and restore. On failure close the wire before draining
// the command: Client.Close alone can wait on a decoder blocked on its queue.
func (src *IMAPSource) listMailboxesLocked(visit func(*imap.ListData)) error {
	limits := src.limits.defaults()
	src.listing.begin(limits.bytes)
	cmd := src.client.List("", "*", &imap.ListOptions{})
	var failure error
	count := 0
	for mb := cmd.Next(); mb != nil; mb = cmd.Next() {
		count++
		if count > limits.items {
			failure = fmt.Errorf("%w: LIST exceeds %d mailboxes", errListingLimit, limits.items)
			_ = src.wire.Close()
			break
		}
		visit(mb)
	}
	err := errors.Join(failure, cmd.Close(), src.listing.end())
	if err != nil {
		if closeErr := src.closeLocked(); closeErr != nil {
			return errors.Join(err, fmt.Errorf("close: %w", closeErr))
		}
	}
	return err
}
