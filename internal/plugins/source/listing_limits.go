package source

import (
	"errors"
	"fmt"
	"math"
	"net"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/zekihan/mailvault/internal/plugins/config"
)

var errListingLimit = errors.New("mail server listing limit exceeded")

type listingLimits struct {
	items int
	bytes int
}

func (l listingLimits) defaults() listingLimits {
	if l.items == 0 {
		l.items = config.DefaultMaxListingItems
	}
	if l.bytes == 0 {
		l.bytes = config.DefaultMaxListingBytes
	}
	return l
}

func parseListingLimits(values map[string]interface{}) (listingLimits, error) {
	var l listingLimits
	for key, dst := range map[string]*int{
		config.SourceConfigMaxListingItems: &l.items,
		config.SourceConfigMaxListingBytes: &l.bytes,
	} {
		// ParseInt also accepts floats. Reject fractions and values that cannot
		// be represented as an int instead of silently weakening the limit.
		if v, ok := values[key].(float64); ok && (math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= float64(^uint(0)>>1) || math.Trunc(v) != v) {
			return l, fmt.Errorf("%s must be a non-negative integer", key)
		}
		if v, ok := values[key].(int64); ok && (v < 0 || uint64(v) > uint64(^uint(0)>>1)) {
			return l, fmt.Errorf("%s is out of range", key)
		}
		v, err := config.ParseInt(values, key, 0)
		if err != nil || v < 0 {
			return l, fmt.Errorf("%s must be a non-negative integer", key)
		}
		*dst = v
	}
	return l.defaults(), nil
}

// boundedSearchUIDs validates the complete result before allocating any UID
// slice. Never call AllUIDs/Nums on server-controlled ranges: their uint32 loop
// can wrap at MaxUint32 in the pinned go-imap version.
func boundedSearchUIDs(data *imap.SearchData, maxItems int) ([]imap.UID, error) {
	if data.All == nil {
		return nil, nil
	}
	if maxItems <= 0 {
		return nil, fmt.Errorf("%w: invalid UID budget", errListingLimit)
	}
	set, ok := data.All.(imap.UIDSet)
	if !ok || set.Dynamic() {
		return nil, errors.New("invalid IMAP SEARCH UID set")
	}
	var total uint64
	for _, r := range set {
		if r.Start == 0 || r.Stop < r.Start {
			return nil, errors.New("invalid IMAP SEARCH UID range")
		}
		n := uint64(r.Stop) - uint64(r.Start) + 1
		if n > uint64(maxItems)-total {
			return nil, fmt.Errorf("%w: SEARCH exceeds %d UIDs", errListingLimit, maxItems)
		}
		total += n
	}
	uids := make([]imap.UID, 0, int(total))
	for _, r := range set {
		for uid := uint64(r.Start); uid <= uint64(r.Stop); uid++ {
			uids = append(uids, imap.UID(uid))
		}
	}
	return uids, nil
}

// listingBudgetConn bounds bytes received from the transport during LIST and
// SEARCH, including TLS framing. Count after Read as IMAP's background reader
// may already be blocked when the command activates its budget. Parser buffers
// may retain one small read from the preceding command.
type listingBudgetConn struct {
	net.Conn
	mu        sync.Mutex
	active    bool
	remaining int
	err       error
}

func (c *listingBudgetConn) begin(maxBytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active, c.remaining, c.err = true, maxBytes, nil
}

func (c *listingBudgetConn) end() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = false
	return c.err
}

func (c *listingBudgetConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.active && len(p)-1 > c.remaining {
		p = p[:c.remaining+1]
	}
	c.mu.Unlock()
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	if c.active {
		if n > c.remaining {
			c.err = fmt.Errorf("%w: response byte budget", errListingLimit)
			err = c.err
			n = c.remaining
			c.remaining = 0
		} else {
			c.remaining -= n
		}
	}
	c.mu.Unlock()
	if errors.Is(err, errListingLimit) {
		_ = c.Close()
	}
	return n, err
}
