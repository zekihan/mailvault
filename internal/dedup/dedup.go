package dedup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
			value = bytes.TrimSpace(value)
			for len(value) == 0 && len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t') {
				value, raw, _ = bytes.Cut(raw, []byte{'\n'})
				value = bytes.TrimSpace(value)
			}
			if len(value) >= 2 && value[0] == '<' && value[len(value)-1] == '>' {
				value = value[1 : len(value)-1]
			}
			return string(value)
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
