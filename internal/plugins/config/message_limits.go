package config

import (
	"fmt"
	"math"
	"strconv"

	"github.com/zekihan/mailvault/internal/plugins"
)

// ParseMessageLimit refuses fractional, overflowing, zero and negative values.
func ParseMessageLimit(values map[string]interface{}) (int64, error) {
	value, exists := values["max_message_bytes"]
	if !exists {
		return plugins.DefaultMaxMessageBytes, nil
	}
	var limit int64
	switch value := value.(type) {
	case int:
		limit = int64(value)
	case int64:
		limit = value
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || value < 1 || value > float64(plugins.MaxMessageBytes) {
			return 0, fmt.Errorf("invalid max_message_bytes")
		}
		limit = int64(value)
	case string:
		var err error
		limit, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid max_message_bytes: %w", err)
		}
	default:
		return 0, fmt.Errorf("invalid max_message_bytes")
	}
	return limit, plugins.ValidateMessageLimit(limit)
}
