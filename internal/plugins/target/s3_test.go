package target

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestS3Target_CustomEndpointUpload(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	requests := make(chan string, 2)
	raw := "Message-ID: <test@example.com>\r\n\r\ntest message\r\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method
		switch r.Method {
		case http.MethodHead:
			if r.URL.Path != "/test-bucket" {
				t.Errorf("bucket path = %q", r.URL.Path)
			}
		case http.MethodPut:
			if !strings.HasPrefix(r.URL.Path, "/test-bucket/backup/") || !strings.HasSuffix(r.URL.Path, "-42.eml") {
				t.Errorf("object path = %q", r.URL.Path)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upload: %v", err)
			}
			if string(body) != raw {
				t.Errorf("uploaded body = %q, want %q", body, raw)
			}
			for header, want := range map[string]string{
				"Content-Type":             "message/rfc822",
				"X-Amz-Meta-Source-Uid":    "42",
				"X-Amz-Meta-Source-Folder": "INBOX",
			} {
				if got := r.Header.Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
			w.Header().Set("ETag", `"test-etag"`)
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	target := &S3Target{
		bucket:   "test-bucket",
		region:   "us-east-1",
		endpoint: server.URL,
		prefix:   "backup/",
	}
	ctx := context.Background()
	if err := target.Initialize(ctx); err != nil {
		t.Fatalf("Initialize() = %v", err)
	}
	if err := target.WriteMessage(ctx, &plugins.Message{UID: "42", Folder: "INBOX", Raw: []byte(raw)}); err != nil {
		t.Fatalf("WriteMessage() = %v", err)
	}
	for _, want := range []string{http.MethodHead, http.MethodPut} {
		select {
		case got := <-requests:
			if got != want {
				t.Errorf("request method = %q, want %q", got, want)
			}
		default:
			t.Errorf("missing %s request", want)
		}
	}
}
