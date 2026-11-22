package plugins

import (
	"bytes"
	"errors"
	"testing"
)

func TestMessageLimitAndValidate(t *testing.T) {
	if got := MessageLimit(0); got != DefaultMaxMessageBytes {
		t.Fatalf("MessageLimit(0) = %d, want default %d", got, DefaultMaxMessageBytes)
	}
	if got := MessageLimit(123); got != 123 {
		t.Fatalf("MessageLimit(123) = %d, want 123", got)
	}
	for _, limit := range []int64{1, MaxMessageBytes} {
		if err := ValidateMessageLimit(limit); err != nil {
			t.Fatalf("ValidateMessageLimit(%d) error: %v", limit, err)
		}
	}
	for _, limit := range []int64{0, -1, MaxMessageBytes + 1} {
		if err := ValidateMessageLimit(limit); err == nil {
			t.Fatalf("ValidateMessageLimit(%d) accepted, want error", limit)
		}
	}
}

func TestCheckMessageSize(t *testing.T) {
	if err := CheckMessageSize(4, 4); err != nil {
		t.Fatalf("size at limit rejected: %v", err)
	}
	for _, size := range []int64{-1, 5} {
		if err := CheckMessageSize(size, 4); !errors.Is(err, ErrMessageTooLarge) {
			t.Fatalf("CheckMessageSize(%d, 4) = %v, want ErrMessageTooLarge", size, err)
		}
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestReadMessage(t *testing.T) {
	raw, err := ReadMessage(bytes.NewReader([]byte("mail")), 4)
	if err != nil || string(raw) != "mail" {
		t.Fatalf("ReadMessage() = %q, %v; want the full message", raw, err)
	}
	if _, err := ReadMessage(bytes.NewReader([]byte("too long")), 4); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversized ReadMessage() = %v, want ErrMessageTooLarge", err)
	}
	readErr := errors.New("read failed")
	if _, err := ReadMessage(failingReader{err: readErr}, 4); !errors.Is(err, readErr) {
		t.Fatalf("ReadMessage() = %v, want the reader error", err)
	}
}
