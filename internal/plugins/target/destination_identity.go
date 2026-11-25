package target

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func destinationFingerprint(values ...string) string {
	data, _ := json.Marshal(values) //nolint:errcheck,errchkjson // marshaling []string cannot fail; hash input must stay stable
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func canonicalDestinationPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	abs := filepath.Clean(absolute)
	missing := make([]string, 0)
	existing := abs
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return abs
		}
		missing = append(missing, filepath.Base(existing))
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return abs
	}
	for _, part := range slices.Backward(missing) {
		resolved = filepath.Join(resolved, part)
	}
	return filepath.Clean(resolved)
}

func canonicalEndpoint(endpoint string) string {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(strings.TrimSpace(endpoint), "/")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String()
}
