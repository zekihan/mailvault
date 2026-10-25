package plugins

import (
	"errors"
	"fmt"
	"io"
)

const DefaultMaxMessageBytes int64 = 16 << 20
const MaxMessageBytes int64 = 512 << 20

var ErrMessageTooLarge = errors.New("message exceeds max_message_bytes")

// MessageLimit also supplies a safe default to callers constructing plugins directly.
func MessageLimit(limit int64) int64 {
	if limit == 0 {
		return DefaultMaxMessageBytes
	}
	return limit
}

func ValidateMessageLimit(limit int64) error {
	if limit < 1 || limit > MaxMessageBytes {
		return fmt.Errorf("max_message_bytes must be between 1 and %d", MaxMessageBytes)
	}
	return nil
}

func CheckMessageSize(size, limit int64) error {
	limit = MessageLimit(limit)
	if size < 0 || size > limit {
		return fmt.Errorf("%w: size %d, limit %d", ErrMessageTooLarge, size, limit)
	}
	return nil
}

// ReadMessage bounds the actual stream even when its advertised size is false.
func ReadMessage(reader io.Reader, limit int64) ([]byte, error) {
	limit = MessageLimit(limit)
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if err := CheckMessageSize(int64(len(raw)), limit); err != nil {
		return nil, err
	}
	return raw, nil
}
