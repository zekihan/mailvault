package dedup

import (
	"testing"
)

func TestComputeContentHash(t *testing.T) {
	raw := []byte("test message content")
	hash := ComputeContentHash(raw)
	if len(hash) != 64 { // SHA-256 hex = 64 chars
		t.Fatalf("ComputeContentHash() length = %d, want 64", len(hash))
	}

	// Same input should produce same hash
	hash2 := ComputeContentHash(raw)
	if hash != hash2 {
		t.Fatalf("ComputeContentHash() not deterministic: %s != %s", hash, hash2)
	}

	// Different input should produce different hash
	hash3 := ComputeContentHash([]byte("different"))
	if hash == hash3 {
		t.Fatal("ComputeContentHash() collision on different input")
	}
}

func TestExtractMessageID(t *testing.T) {
	tests := []struct {
		name     string
		raw      []byte
		expected string
	}{
		{
			name:     "standard format",
			raw:      []byte("Message-ID: <test@example.com>\n\nBody"),
			expected: "test@example.com",
		},
		{
			name:     "lowercase header",
			raw:      []byte("message-id: <lower@example.com>\n\nBody"),
			expected: "lower@example.com",
		},
		{
			name:     "no brackets",
			raw:      []byte("Message-ID: no-brackets@example.com\n\nBody"),
			expected: "no-brackets@example.com",
		},
		{
			name:     "folded before identifier",
			raw:      []byte("Message-ID:\r\n\t<folded@example.invalid>\r\nSubject: Test\r\n\r\nBody"),
			expected: "folded@example.invalid",
		},
		{
			name:     "folded after whitespace",
			raw:      []byte("Message-ID: \n <folded@example.invalid>\n\nBody"),
			expected: "folded@example.invalid",
		},
		{
			name:     "empty value followed by body",
			raw:      []byte("Message-ID:\r\n\r\n <body@example.invalid>"),
			expected: "",
		},
		{
			name:     "malformed folded identifier",
			raw:      []byte("Message-ID: <folded\n	example.com>\n\nBody"),
			expected: "<folded",
		},
		{
			name:     "missing",
			raw:      []byte("Subject: Test\n\nBody"),
			expected: "",
		},
		{
			name:     "empty message",
			raw:      []byte(""),
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractMessageID(tt.raw)
			if got != tt.expected {
				t.Fatalf("ExtractMessageID() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestDedupKey_Key(t *testing.T) {
	tests := []struct {
		key      DedupKey
		expected string
	}{
		{DedupKey{MessageID: "test@example.com"}, "mid:test@example.com"},
		{DedupKey{ContentHash: "abc123"}, "hash:abc123"},
		{DedupKey{MessageID: "mid", ContentHash: "hash"}, "mid:mid"},
		{DedupKey{}, ""},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			if got := tt.key.Key(); got != tt.expected {
				t.Fatalf("Key() = %q, want %q", got, tt.expected)
			}
		})
	}
}
