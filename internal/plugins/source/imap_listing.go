package source

import (
	"errors"
	"fmt"

	"github.com/emersion/go-imap/v2"
)

// listMailboxesLocked streams even non-selectable entries through the same
// budget for discovery and restore. On failure close the wire before draining
// the command: Client.Close alone can wait on a decoder blocked on its queue.
func (s *IMAPSource) listMailboxesLocked(visit func(*imap.ListData)) error {
	limits := s.limits.defaults()
	s.listing.begin(limits.bytes)
	cmd := s.client.List("", "*", &imap.ListOptions{})
	var failure error
	count := 0
	for mb := cmd.Next(); mb != nil; mb = cmd.Next() {
		count++
		if count > limits.items {
			failure = fmt.Errorf("%w: LIST exceeds %d mailboxes", errListingLimit, limits.items)
			_ = s.wire.Close()
			break
		}
		visit(mb)
	}
	err := errors.Join(failure, cmd.Close(), s.listing.end())
	if err != nil {
		_ = s.closeLocked()
	}
	return err
}
