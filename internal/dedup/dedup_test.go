package dedup

import (
	"context"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
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
			name:     "folded header",
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

// mockStateStore implements StateStore for testing.
type mockStateStore struct {
	existsByUID      map[string]bool
	foundByMessageID map[string][]MessageRecord
	foundByHash      map[string][]MessageRecord
}

func (m *mockStateStore) MessageExists(ctx context.Context, sourceName, folder, uid string) (bool, error) {
	key := sourceName + "/" + folder + "/" + uid
	return m.existsByUID[key], nil
}

func (m *mockStateStore) FindByMessageID(ctx context.Context, messageID string) ([]MessageRecord, error) {
	return m.foundByMessageID[messageID], nil
}

func (m *mockStateStore) FindByContentHash(ctx context.Context, contentHash string) ([]MessageRecord, error) {
	return m.foundByHash[contentHash], nil
}

func TestDedupTracker_ShouldSkip(t *testing.T) {
	ctx := context.Background()
	mock := &mockStateStore{
		existsByUID: map[string]bool{
			"gmail/INBOX/999": true,
		},
		foundByMessageID: map[string][]MessageRecord{
			"<dup@example.com>": {{SourceName: "yahoo", Folder: "INBOX", UID: "5"}},
		},
		foundByHash: map[string][]MessageRecord{
			"hash123": {{SourceName: "outlook", Folder: "INBOX", UID: "10"}},
		},
	}

	tracker := NewDedupTracker(mock, "gmail")

	// Test 1: Already seen in this run (in-memory)
	msg1 := &plugins.Message{UID: "1", MessageID: "<new@example.com>", ContentHash: "hash-new"}
	skip, err := tracker.ShouldSkip(ctx, msg1, "INBOX")
	if err != nil {
		t.Fatalf("ShouldSkip() error: %v", err)
	}
	if skip {
		t.Fatal("ShouldSkip() = true for new message, want false")
	}

	// Mark as seen
	tracker.MarkSeen(msg1)

	// Should skip now
	skip, err = tracker.ShouldSkip(ctx, msg1, "INBOX")
	if err != nil {
		t.Fatalf("ShouldSkip() error: %v", err)
	}
	if !skip {
		t.Fatal("ShouldSkip() = false for already seen message, want true")
	}

	// Test 2: Exists in state by UID
	msg2 := &plugins.Message{UID: "999", MessageID: "<other@example.com>", ContentHash: "hash-other"}
	skip, err = tracker.ShouldSkip(ctx, msg2, "INBOX")
	if err != nil {
		t.Fatalf("ShouldSkip() error: %v", err)
	}
	if !skip {
		t.Fatal("ShouldSkip() = false for UID in state, want true")
	}

	// Test 3: Cross-source dedup by Message-ID
	msg3 := &plugins.Message{UID: "999", MessageID: "<dup@example.com>", ContentHash: "hash-new2"}
	skip, err = tracker.ShouldSkip(ctx, msg3, "INBOX")
	if err != nil {
		t.Fatalf("ShouldSkip() error: %v", err)
	}
	if !skip {
		t.Fatal("ShouldSkip() = false for Message-ID in state, want true")
	}

	// Test 4: Cross-source dedup by Content-Hash
	msg4 := &plugins.Message{UID: "999", MessageID: "<unique@example.com>", ContentHash: "hash123"}
	skip, err = tracker.ShouldSkip(ctx, msg4, "INBOX")
	if err != nil {
		t.Fatalf("ShouldSkip() error: %v", err)
	}
	if !skip {
		t.Fatal("ShouldSkip() = false for Content-Hash in state, want true")
	}

	// Test 5: New message (no dedup hit)
	msg5 := &plugins.Message{UID: "2", MessageID: "<unique2@example.com>", ContentHash: "hash-new3"}
	skip, err = tracker.ShouldSkip(ctx, msg5, "INBOX")
	if err != nil {
		t.Fatalf("ShouldSkip() error: %v", err)
	}
	if skip {
		t.Fatal("ShouldSkip() = true for new message, want false")
	}

	// Test 6: nil state (no cross-source dedup)
	tracker2 := NewDedupTracker(nil, "gmail")
	msg6 := &plugins.Message{UID: "1", MessageID: "<dup@example.com>", ContentHash: "hash123"}
	skip, err = tracker2.ShouldSkip(ctx, msg6, "INBOX")
	if err != nil {
		t.Fatalf("ShouldSkip() error: %v", err)
	}
	if skip {
		t.Fatal("ShouldSkip() = true with nil state, want false (only in-memory)")
	}
}

func TestDedupTracker_SeenCount(t *testing.T) {
	tracker := NewDedupTracker(nil, "test")

	if tracker.SeenCount() != 0 {
		t.Fatal("SeenCount() = non-zero initially")
	}

	tracker.MarkSeen(&plugins.Message{MessageID: "<a@example.com>"})
	if tracker.SeenCount() != 1 {
		t.Fatal("SeenCount() = 1 after one message")
	}

	tracker.MarkSeen(&plugins.Message{MessageID: "<b@example.com>"})
	if tracker.SeenCount() != 2 {
		t.Fatal("SeenCount() = 2 after two messages")
	}

	// Duplicate should not increase count
	tracker.MarkSeen(&plugins.Message{MessageID: "<a@example.com>"})
	if tracker.SeenCount() != 2 {
		t.Fatal("SeenCount() should not increase for duplicate")
	}
}
