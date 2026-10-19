package target

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/zekihan/mailvault/internal/plugins"
)

const maxS3MetadataBytes = 2048

// Preserve ordinary metadata verbatim. If its UTF-8 key/value byte total would
// exceed S3's budget, replace the largest unbounded values with explicit digest
// fields. The raw RFC822 object and stable object key are never changed here.
func s3ObjectMetadata(msg *plugins.Message, contentHash [32]byte) map[string]string {
	metadata := map[string]string{
		"source-folder": msg.Folder,
		"source-uid":    msg.UID,
		"message-id":    msg.MessageID,
		"content-hash":  hex.EncodeToString(contentHash[:]),
		"internal-date": msg.InternalDate,
		"size":          fmt.Sprintf("%d", msg.Size),
	}
	size := 0
	for key, value := range metadata {
		size += len(key) + len(value)
	}
	if size <= maxS3MetadataBytes {
		return metadata
	}
	keys := []string{"source-folder", "source-uid", "message-id", "internal-date"}
	sort.Slice(keys, func(i, j int) bool {
		if len(metadata[keys[i]]) == len(metadata[keys[j]]) {
			return keys[i] < keys[j]
		}
		return len(metadata[keys[i]]) > len(metadata[keys[j]])
	})
	for _, key := range keys {
		if size <= maxS3MetadataBytes {
			break
		}
		value := metadata[key]
		const suffix = "-sha256"
		if len(value) <= sha256.Size*2+len(suffix) {
			continue
		}
		digest := sha256.Sum256([]byte(value))
		delete(metadata, key)
		metadata[key+suffix] = hex.EncodeToString(digest[:])
		size += sha256.Size*2 + len(suffix) - len(value)
	}
	return metadata
}
