package plugins

import (
	"context"
	"errors"
	"time"
)

// newIMapSource is a placeholder for the IMAP source factory.
func newIMapSource(config map[string]interface{}) (Source, error) {
	return nil, errors.New("imap source not yet implemented")
}

// newPOP3Source is a placeholder for the POP3 source factory.
func newPOP3Source(config map[string]interface{}) (Source, error) {
	return nil, errors.New("pop3 source not yet implemented")
}

// newMaildirTarget is a placeholder for the Maildir target factory.
func newMaildirTarget(config map[string]interface{}) (Target, error) {
	return nil, errors.New("maildir target not yet implemented")
}

// newMboxTarget is a placeholder for the mbox target factory.
func newMboxTarget(config map[string]interface{}) (Target, error) {
	return nil, errors.New("mbox target not yet implemented")
}

// newS3Target is a placeholder for the S3 target factory.
func newS3Target(config map[string]interface{}) (Target, error) {
	return nil, errors.New("s3 target not yet implemented")
}

// ParseDuration parses a duration from config (supports string or int seconds).
func ParseDuration(config map[string]interface{}, key string, defaultVal time.Duration) (time.Duration, error) {
	val, ok := config[key]
	if !ok {
		return defaultVal, nil
	}

	switch v := val.(type) {
	case string:
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, err
		}
		return d, nil
	case int:
		return time.Duration(v) * time.Second, nil
	case int64:
		return time.Duration(v) * time.Second, nil
	case float64:
		return time.Duration(v) * time.Second, nil
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
		return int(v), nil
	case float64:
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

// contextWithTimeout returns a context with timeout if deadline is set.
func contextWithTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}