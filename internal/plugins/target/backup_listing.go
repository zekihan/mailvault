package target

import (
	"context"
	"errors"
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

func parseBackupListingLimits(configMap map[string]any) (backupListingLimits, error) {
	limits := backupListingLimits{defaultMaxBackupEntries, defaultMaxBackupLocatorBytes}
	for key, dest := range map[string]*int64{config.TargetConfigMaxListingEntries: &limits.entries, config.TargetConfigMaxListingLocatorBytes: &limits.locatorBytes} {
		if err := applyBackupListingValue(configMap, key, dest); err != nil {
			return limits, err
		}
	}
	return limits, nil
}

func applyBackupListingValue(configMap map[string]any, key string, dest *int64) error {
	value, ok := configMap[key]
	if !ok {
		return nil
	}
	count, err := parseBackupListingValue(value)
	if err != nil || count < 0 {
		return fmt.Errorf("%s: expected a nonnegative integer", key)
	}
	if count > 0 {
		*dest = count
	}
	return nil
}

func parseBackupListingValue(value any) (int64, error) {
	switch parsed := value.(type) {
	case int:
		return int64(parsed), nil
	case int64:
		return parsed, nil
	case string:
		return strconv.ParseInt(parsed, 10, 64)
	case float64:
		return parseBackupListingFloat(parsed)
	default:
		return 0, errors.New("invalid type")
	}
}

func parseBackupListingFloat(parsed float64) (int64, error) {
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed >= 9223372036854775808.0 {
		return 0, errors.New("out of range")
	}
	count := int64(parsed)
	if float64(count) != parsed {
		return 0, errors.New("fractional value")
	}
	return count, nil
}

func (limits backupListingLimits) budget() *backupListingBudget {
	if limits.entries == 0 {
		limits.entries = defaultMaxBackupEntries
	}
	if limits.locatorBytes == 0 {
		limits.locatorBytes = defaultMaxBackupLocatorBytes
	}
	return &backupListingBudget{limits: limits}
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
