package config

import (
	"errors"
	"math"
	"strconv"
	"time"
)

// ParseDuration accepts duration values, duration strings, and numeric seconds.
func ParseDuration(config map[string]interface{}, key string, defaultVal time.Duration) (time.Duration, error) {
	val, ok := config[key]
	if !ok {
		return defaultVal, nil
	}

	switch v := val.(type) {
	case time.Duration:
		return v, nil
	case string:
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, err
		}
		return d, nil
	case int:
		if int64(v) > math.MaxInt64/int64(time.Second) || int64(v) < math.MinInt64/int64(time.Second) {
			return 0, errors.New("duration out of range")
		}
		return time.Duration(v) * time.Second, nil
	case int64:
		if v > math.MaxInt64/int64(time.Second) || v < math.MinInt64/int64(time.Second) {
			return 0, errors.New("duration out of range")
		}
		return time.Duration(v) * time.Second, nil
	case float64:
		// Parse decimal seconds like a duration string to avoid rounding the
		// whole duration before truncating it to nanoseconds.
		d, err := time.ParseDuration(strconv.FormatFloat(v, 'f', -1, 64) + "s")
		if err != nil {
			return 0, errors.New("duration out of range")
		}
		return d, nil
	default:
		return 0, errors.New("invalid duration format")
	}
}

// ParseInt parses an int from config.
func ParseInt(config map[string]interface{}, key string, defaultVal int) (int, error) {
	val, ok := config[key]
	if !ok {
		return defaultVal, nil
	}

	switch v := val.(type) {
	case int:
		return v, nil
	case int64:
		if int64(int(v)) != v {
			return 0, errors.New("invalid int format")
		}
		return int(v), nil
	case float64:
		limit := math.Ldexp(1, strconv.IntSize-1)
		if math.IsNaN(v) || v < -limit || v >= limit || math.Trunc(v) != v {
			return 0, errors.New("invalid int format")
		}
		return int(v), nil
	default:
		return 0, errors.New("invalid int format")
	}
}

// ParseString parses a string from config.
func ParseString(config map[string]interface{}, key string, defaultVal string) (string, error) {
	val, ok := config[key]
	if !ok {
		return defaultVal, nil
	}

	s, ok := val.(string)
	if !ok {
		return "", errors.New("invalid string format")
	}
	return s, nil
}

// ParseStringSlice parses a []string from config.
func ParseStringSlice(config map[string]interface{}, key string) ([]string, error) {
	val, ok := config[key]
	if !ok {
		return nil, nil
	}

	switch v := val.(type) {
	case []interface{}:
		result := make([]string, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, errors.New("invalid string slice format")
			}
			result[i] = s
		}
		return result, nil
	case []string:
		return v, nil
	default:
		return nil, errors.New("invalid string slice format")
	}
}

// ParseBool parses a bool from config.
func ParseBool(config map[string]interface{}, key string, defaultVal bool) (bool, error) {
	val, ok := config[key]
	if !ok {
		return defaultVal, nil
	}

	b, ok := val.(bool)
	if !ok {
		return false, errors.New("invalid bool format")
	}
	return b, nil
}
