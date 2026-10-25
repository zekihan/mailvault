package dedup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	"github.com/zekihan/mailvault/internal/plugins"
)

// ComputeContentHash returns SHA-256 hash of message raw bytes.
func ComputeContentHash(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// ExtractMessageID extracts the Message-ID header from raw RFC822 message.
func ExtractMessageID(raw []byte) string {
	for len(raw) > 0 {
		line, rest, _ := bytes.Cut(raw, []byte{'\n'})
		raw = rest
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			break
		}
		key, value, found := bytes.Cut(line, []byte{':'})
		if found && bytes.EqualFold(key, []byte("Message-ID")) {
			text := strings.TrimSpace(string(value))
			if strings.HasPrefix(text, "<") && strings.HasSuffix(text, ">") {
				return text[1 : len(text)-1]
			}
			return text
		}
	}
	return ""
}

// DedupKey represents a deduplication key for a message.
type DedupKey struct {
	MessageID   string
	ContentHash string
}

// Key returns a combined key for deduplication.
func (k DedupKey) Key() string {
	if k.MessageID != "" {
		return "mid:" + k.MessageID
	}
	if k.ContentHash != "" {
		return "hash:" + k.ContentHash
	}
	return ""
}

// DedupTracker tracks seen messages for deduplication within a sync run.
type DedupTracker struct {
	mu         sync.Mutex
	seen       map[string]bool
	state      StateStore
	sourceName string
}

// StateStore is the interface for checking persisted state.
type StateStore interface {
	MessageExists(ctx context.Context, sourceName, folder, uid string) (bool, error)
	FindByMessageID(ctx context.Context, messageID string) ([]MessageRecord, error)
	FindByContentHash(ctx context.Context, contentHash string) ([]MessageRecord, error)
}

// MessageRecord represents a stored message (subset for dedup).
type MessageRecord struct {
	SourceName string
	Folder     string
	UID        string
}

// NewDedupTracker creates a new deduplication tracker.
func NewDedupTracker(state StateStore, sourceName string) *DedupTracker {
	return &DedupTracker{
		seen:       make(map[string]bool),
		state:      state,
		sourceName: sourceName,
	}
}

// ShouldSkip checks if a message should be skipped (already seen or in state).
// Returns true if the message is a duplicate and should be skipped.
func (d *DedupTracker) ShouldSkip(ctx context.Context, msg *plugins.Message, folder string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Check in-memory tracker first (within this run)
	key := DedupKey{MessageID: msg.MessageID, ContentHash: msg.ContentHash}.Key()
	if key != "" {
		if d.seen[key] {
			return true, nil
		}
	}

	// Check persisted state by UID (same source, same folder)
	if d.state != nil {
		exists, err := d.state.MessageExists(ctx, d.sourceName, folder, msg.UID)
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}

		// Check cross-source deduplication by Message-ID
		if msg.MessageID != "" {
			found, err := d.state.FindByMessageID(ctx, msg.MessageID)
			if err != nil {
				return false, err
			}
			if len(found) > 0 {
				// Found same Message-ID from any source
				return true, nil
			}
		}

		// Check cross-source deduplication by Content-Hash
		if msg.ContentHash != "" {
			found, err := d.state.FindByContentHash(ctx, msg.ContentHash)
			if err != nil {
				return false, err
			}
			if len(found) > 0 {
				return true, nil
			}
		}
	}

	// Mark as seen in this run
	if key != "" {
		d.seen[key] = true
	}

	return false, nil
}

// MarkSeen marks a message as seen in this run.
func (d *DedupTracker) MarkSeen(msg *plugins.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := DedupKey{MessageID: msg.MessageID, ContentHash: msg.ContentHash}.Key()
	if key != "" {
		d.seen[key] = true
	}
}

// SeenCount returns the number of unique messages seen in this run.
func (d *DedupTracker) SeenCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}
