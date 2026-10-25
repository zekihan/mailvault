package target

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/config"
)

const defaultMaxBackupEntries = 1_000_000
const defaultMaxBackupLocatorBytes = 64 << 20
const backupDirectoryBatchSize = 128

type backupListingLimits struct{ entries, locatorBytes int64 }
type backupListingBudget struct {
	limits                backupListingLimits
	entries, locatorBytes int64
}

func parseBackupListingLimits(configMap map[string]interface{}) (backupListingLimits, error) {
	limits := backupListingLimits{defaultMaxBackupEntries, defaultMaxBackupLocatorBytes}
	for key, dest := range map[string]*int64{config.TargetConfigMaxListingEntries: &limits.entries, config.TargetConfigMaxListingLocatorBytes: &limits.locatorBytes} {
		value, ok := configMap[key]
		if !ok {
			continue
		}
		var n int64
		var err error
		switch v := value.(type) {
		case int:
			n = int64(v)
		case int64:
			n = v
		case string:
			n, err = strconv.ParseInt(v, 10, 64)
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= 9223372036854775808.0 {
				return limits, fmt.Errorf("%s: expected a nonnegative integer", key)
			}
			n = int64(v)
			if float64(n) != v {
				err = fmt.Errorf("fractional value")
			}
		default:
			err = fmt.Errorf("invalid type")
		}
		if err != nil || n < 0 {
			return limits, fmt.Errorf("%s: expected a nonnegative integer", key)
		}
		if n > 0 {
			*dest = n
		}
	}
	return limits, nil
}

func (l backupListingLimits) budget() *backupListingBudget {
	if l.entries == 0 {
		l.entries = defaultMaxBackupEntries
	}
	if l.locatorBytes == 0 {
		l.locatorBytes = defaultMaxBackupLocatorBytes
	}
	return &backupListingBudget{limits: l}
}

func (b *backupListingBudget) add(locator string) error {
	if b.entries >= b.limits.entries || int64(len(locator)) > b.limits.locatorBytes-b.locatorBytes {
		return fmt.Errorf("%w (max_listing_entries=%d, max_listing_locator_bytes=%d)", plugins.ErrBackupListingLimit, b.limits.entries, b.limits.locatorBytes)
	}
	b.entries++
	b.locatorBytes += int64(len(locator))
	return nil
}

// collectBackupMessages is the bounded compatibility API. Operational consumers
// use WalkBackupMessages so pages are consumed before fetching the next page.
func collectBackupMessages(ctx context.Context, walk func(context.Context, func(plugins.BackupMessage) error) error) ([]plugins.BackupMessage, error) {
	var result []plugins.BackupMessage
	err := walk(ctx, func(entry plugins.BackupMessage) error { result = append(result, entry); return nil })
	if err != nil {
		return nil, err
	}
	return result, nil
}
