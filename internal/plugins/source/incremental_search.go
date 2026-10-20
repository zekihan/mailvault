package source

import (
	"sort"
	"strconv"

	"github.com/emersion/go-imap/v2"
)

// Exclude only proven deliveries, preserving holes from failures and date filters.
// Bound command size for servers with conservative command-line limits.
func excludeDelivered(criteria *imap.SearchCriteria, known map[string]bool) {
	ids := make([]uint32, 0, len(known))
	for value, delivered := range known {
		id, err := strconv.ParseUint(value, 10, 32)
		if delivered && err == nil && id != 0 {
			ids = append(ids, uint32(id))
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var set imap.UIDSet
	for _, id := range ids {
		set.AddNum(imap.UID(id))
	}
	if len(set) == 0 || len(set.String()) > 4096 {
		return
	}
	criteria.Not = append(criteria.Not, imap.SearchCriteria{UID: []imap.UIDSet{set}})
}
