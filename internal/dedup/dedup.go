package dedup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/zekihan/mailvault/internal/plugins"
)

// ComputeContentHash returns SHA-256 hash of message raw bytes.
func ComputeContentHash(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// ExtractMessageID extracts the Message-ID header from raw RFC822 message.
func ExtractMessageID(raw []byte) string {
	// Simple parser for Message-ID header
	// Message-ID: <value>
	lines := strings.Split(string(raw), "\n")
	inHeader := true
	for _, line := range lines {
		if inHeader {
			if line == "" || line == "\r" {
				inHeader = false
				continue
			}
			if strings.HasPrefix(line, "Message-ID:") || strings.HasPrefix(line, "Message-Id:") || strings.HasPrefix(line, "message-id:") {
				value := strings.TrimSpace(line[len("Message-ID:"):])
				// Extract content between < > if present
				value = strings.TrimSpace(value)
				if strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">") {
					return value[1 : len(value)-1]
				}
				return value
			}
			// Handle continuation lines (folded headers)
			if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				continue
			}
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
	key := DedupKey{MessageID: msg.MessageID, ContentHash: msg.ContentHash}.Key()
	if key != "" {
		d.seen[key] = true
	}
}

// SeenCount returns the number of unique messages seen in this run.
func (d *DedupTracker) SeenCount() int {
	return len(d.seen)
}