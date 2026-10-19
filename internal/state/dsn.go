package state

import (
	"net/url"
	"path/filepath"
	"strings"
)

// SQLite interprets query parameters even in plain filename DSNs. Encode the
// absolute filename separately so literal ?, # and % cannot change its target.
func stateDSN(path string, readOnly bool) (string, error) {
	params := url.Values{"_fk": {"1"}, "_busy_timeout": {"5000"}}
	if readOnly {
		params.Set("mode", "ro")
		params.Set("_query_only", "1")
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
