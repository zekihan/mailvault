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

func (limits listingLimits) defaults() listingLimits {
	if limits.items == 0 {
		limits.items = config.DefaultMaxListingItems
	}
	if limits.bytes == 0 {
		limits.bytes = config.DefaultMaxListingBytes
	}
	return limits
}

func parseListingLimits(values map[string]any) (listingLimits, error) {
	var limits listingLimits
	for key, dst := range map[string]*int{
		config.SourceConfigMaxListingItems: &limits.items,
		config.SourceConfigMaxListingBytes: &limits.bytes,
	} {
		// ParseInt also accepts floats. Reject fractions and values that cannot
		// be represented as an int instead of silently weakening the limit.
		if v, ok := values[key].(float64); ok && (math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= float64(^uint(0)>>1) || math.Trunc(v) != v) {
			return limits, fmt.Errorf("%s must be a non-negative integer", key)
		}
		if v, ok := values[key].(int64); ok && (v < 0 || uint64(v) > uint64(^uint(0)>>1)) {
			return limits, fmt.Errorf("%s is out of range", key)
		}
		v, err := config.ParseInt(values, key, 0)
		if err != nil || v < 0 {
			return limits, fmt.Errorf("%s must be a non-negative integer", key)
		}
		*dst = v
	}
	return limits.defaults(), nil
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
			if uid > uint64(^uint32(0)) {
				return nil, fmt.Errorf("%w: UID %d out of range", errListingLimit, uid)
			}
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

func (conn *listingBudgetConn) Read(p []byte) (int, error) {
	conn.mu.Lock()
	if conn.active && len(p)-1 > conn.remaining {
		p = p[:conn.remaining+1]
	}
	conn.mu.Unlock()
	count, err := conn.Conn.Read(p)
	conn.mu.Lock()
	if conn.active {
		if count > conn.remaining {
			conn.err = fmt.Errorf("%w: response byte budget", errListingLimit)
			err = conn.err
			count = conn.remaining
			conn.remaining = 0
		} else {
			conn.remaining -= count
		}
	}
	conn.mu.Unlock()
	if errors.Is(err, errListingLimit) {
		_ = conn.Close()
	}
	return count, err
}

func (conn *listingBudgetConn) begin(maxBytes int) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.active, conn.remaining, conn.err = true, maxBytes, nil
}

func (conn *listingBudgetConn) end() error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.active = false
	return conn.err
}
