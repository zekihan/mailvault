package target

import (
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

const s3CustomRaw = "Message-ID: <test@example.com>\r\n\r\ntest message\r\n"

type s3CustomUploadRecorder struct {
	requests chan string
	keys     chan string
	folders  chan string
	hashes   chan string
	bodies   chan string
}

func (recorder *s3CustomUploadRecorder) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	recorder.requests <- r.Method
	switch r.Method {
	case http.MethodHead:
		if r.URL.Path != "/test-bucket" {
			t.Errorf("bucket path = %q", r.URL.Path)
		}
	case http.MethodPut:
		recorder.handlePut(t, w, r)
	default:
		t.Errorf("unexpected method %s", r.Method)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (recorder *s3CustomUploadRecorder) handlePut(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	recorder.keys <- r.URL.Path
	if !strings.HasPrefix(r.URL.Path, "/test-bucket/backup/") || !strings.HasSuffix(r.URL.Path, "-42.eml") {
		t.Errorf("object path = %q", r.URL.Path)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read upload: %v", err)
	}
	recorder.bodies <- string(body)
	recorder.folders <- r.Header.Get("X-Amz-Meta-Source-Folder")
	recorder.hashes <- r.Header.Get("X-Amz-Meta-Content-Hash")
	for header, want := range map[string]string{
		"Content-Type":          "message/rfc822",
		"X-Amz-Meta-Source-Uid": "42",
	} {
		if got := r.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	w.Header().Set("ETag", `"test-etag"`)
}

func openS3CustomTarget(t *testing.T, endpoint string) *S3Target {
	t.Helper()
	target := &S3Target{
		bucket:   "test-bucket",
		region:   "us-east-1",
		endpoint: endpoint,
		prefix:   "backup/",
	}
	if err := target.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() = %v", err)
	}
	return target
}

func writeS3CustomMessages(t *testing.T, target *S3Target) string {
	t.Helper()
	ctx := t.Context()
	msg := &plugins.Message{UID: "42", Folder: "INBOX", Raw: []byte(s3CustomRaw), ContentHash: "stale"}
	if err := target.WriteMessage(ctx, msg); err != nil {
		t.Fatalf("WriteMessage() = %v", err)
	}
	if err := target.WriteMessage(ctx, msg); err != nil {
		t.Fatalf("retry WriteMessage() = %v", err)
	}
	if err := target.WriteMessage(ctx, &plugins.Message{UID: "42", Folder: "Archive", Raw: []byte(s3CustomRaw)}); err != nil {
		t.Fatal(err)
	}
	return writeS3CustomChangedMessages(t, target)
}

func writeS3CustomChangedMessages(t *testing.T, target *S3Target) string {
	t.Helper()
	changed := s3CustomRaw + "changed content\r\n"
	if err := target.WriteMessage(t.Context(), &plugins.Message{UID: "42", Folder: "INBOX", Raw: []byte(changed), ContentHash: "stale"}); err != nil {
		t.Fatal(err)
	}
	return changed
}

func checkS3CustomRequests(t *testing.T, recorder *s3CustomUploadRecorder) {
	t.Helper()
	for _, want := range []string{http.MethodHead, http.MethodPut, http.MethodPut, http.MethodPut, http.MethodPut} {
		select {
		case got := <-recorder.requests:
			if got != want {
				t.Errorf("request method = %q, want %q", got, want)
			}
		default:
			t.Errorf("missing %s request", want)
		}
	}
}

func checkS3CustomUploads(t *testing.T, recorder *s3CustomUploadRecorder, changed string) {
	t.Helper()
	k1, k2, k3, k4 := <-recorder.keys, <-recorder.keys, <-recorder.keys, <-recorder.keys
	if k1 != k2 || k1 == k3 || k1 == k4 {
		t.Fatalf("keys retry=%q distinct-folder=%q distinct-content=%q", k1, k3, k4)
	}
	for i, wantFolder := range []string{"INBOX", "INBOX", "Archive", "INBOX"} {
		checkS3CustomUpload(t, recorder, i, wantFolder, changed)
	}
}

func checkS3CustomUpload(t *testing.T, recorder *s3CustomUploadRecorder, index int, wantFolder, changed string) {
	t.Helper()
	wantBody := s3CustomRaw
	if index == 3 {
		wantBody = changed
	}
	if got := <-recorder.bodies; got != wantBody {
		t.Errorf("upload %d body = %q, want %q", index, got, wantBody)
	}
	if got := <-recorder.folders; got != wantFolder {
		t.Errorf("upload %d folder = %q, want %q", index, got, wantFolder)
	}
	if got, want := <-recorder.hashes, fmt.Sprintf("%x", sha256.Sum256([]byte(wantBody))); got != want {
		t.Errorf("upload %d content hash = %q, want %q", index, got, want)
	}
}

func TestS3Target_CustomEndpointUpload(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	recorder := &s3CustomUploadRecorder{requests: make(chan string, 5), keys: make(chan string, 4), folders: make(chan string, 4), hashes: make(chan string, 4), bodies: make(chan string, 4)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { recorder.serve(t, w, r) }))
	defer server.Close()

	target := openS3CustomTarget(t, server.URL)
	changed := writeS3CustomMessages(t, target)
	checkS3CustomRequests(t, recorder)
	checkS3CustomUploads(t, recorder, changed)
}
