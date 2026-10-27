package state

import (
	"net/url"
	"path/filepath"
	"strings"
)

// Retain the opened filename even if the caller later changes directories.
func stateFilename(path string, allowMemory bool) (string, error) {
	if allowMemory && path == ":memory:" {
		return path, nil
	}
	return filepath.Abs(path)
}

// SQLite interprets query parameters even in plain filename DSNs. Encode the
// absolute filename separately so literal ?, # and % cannot change its target.
func stateDSN(path string, readOnly bool) (string, error) {
	params := url.Values{"_pragma": {"foreign_keys(1)", "busy_timeout(5000)"}}
	// Use rollback journaling deliberately: WAL needs shared-memory support
	// and does not remove SQLite's single-writer limit. Do not change journal
	// mode when opening a read-only store or an in-memory database.
	if !readOnly && path != ":memory:" {
		params.Add("_pragma", "journal_mode(DELETE)")
	}
	if readOnly {
		params.Set("mode", "ro")
		params.Add("_pragma", "query_only(1)")
	}
	if path == ":memory:" && !readOnly {
		return ":memory:?" + params.Encode(), nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	uriPath := filepath.ToSlash(absolute)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath, RawQuery: params.Encode()}
	return u.String(), nil
}
