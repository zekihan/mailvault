package target

import (
	"context"
	"crypto/sha256"
	"fmt"
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

	requests := make(chan string, 5)
	keys := make(chan string, 4)
	folders := make(chan string, 4)
	hashes := make(chan string, 4)
	bodies := make(chan string, 4)
	raw := "Message-ID: <test@example.com>\r\n\r\ntest message\r\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method
		switch r.Method {
		case http.MethodHead:
			if r.URL.Path != "/test-bucket" {
				t.Errorf("bucket path = %q", r.URL.Path)
			}
		case http.MethodPut:
			keys <- r.URL.Path
			if !strings.HasPrefix(r.URL.Path, "/test-bucket/backup/") || !strings.HasSuffix(r.URL.Path, "-42.eml") {
				t.Errorf("object path = %q", r.URL.Path)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upload: %v", err)
			}
			bodies <- string(body)
			folders <- r.Header.Get("X-Amz-Meta-Source-Folder")
			hashes <- r.Header.Get("X-Amz-Meta-Content-Hash")
			for header, want := range map[string]string{
				"Content-Type":          "message/rfc822",
				"X-Amz-Meta-Source-Uid": "42",
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
	msg := &plugins.Message{UID: "42", Folder: "INBOX", Raw: []byte(raw), ContentHash: "stale"}
	if err := target.WriteMessage(ctx, msg); err != nil {
		t.Fatalf("WriteMessage() = %v", err)
	}
	if err := target.WriteMessage(ctx, msg); err != nil {
		t.Fatalf("retry WriteMessage() = %v", err)
	}
	if err := target.WriteMessage(ctx, &plugins.Message{UID: "42", Folder: "Archive", Raw: []byte(raw)}); err != nil {
		t.Fatal(err)
	}
	changed := raw + "changed content\r\n"
	if err := target.WriteMessage(ctx, &plugins.Message{UID: "42", Folder: "INBOX", Raw: []byte(changed), ContentHash: "stale"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{http.MethodHead, http.MethodPut, http.MethodPut, http.MethodPut, http.MethodPut} {
		select {
		case got := <-requests:
			if got != want {
				t.Errorf("request method = %q, want %q", got, want)
			}
		default:
			t.Errorf("missing %s request", want)
		}
	}
	k1, k2, k3, k4 := <-keys, <-keys, <-keys, <-keys
	if k1 != k2 || k1 == k3 || k1 == k4 {
		t.Fatalf("keys retry=%q distinct-folder=%q distinct-content=%q", k1, k3, k4)
	}
	for i, wantFolder := range []string{"INBOX", "INBOX", "Archive", "INBOX"} {
		wantBody := raw
		if i == 3 {
			wantBody = changed
		}
		if got := <-bodies; got != wantBody {
			t.Errorf("upload %d body = %q, want %q", i, got, wantBody)
		}
		if got := <-folders; got != wantFolder {
			t.Errorf("upload %d folder = %q, want %q", i, got, wantFolder)
		}
		if got, want := <-hashes, fmt.Sprintf("%x", sha256.Sum256([]byte(wantBody))); got != want {
			t.Errorf("upload %d content hash = %q, want %q", i, got, want)
		}
	}
}
