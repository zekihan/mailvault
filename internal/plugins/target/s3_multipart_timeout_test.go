package target

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
)

type multipartTimeoutFixture struct {
	phase   string
	entered chan struct{}
	release chan struct{}
}

func (fixture *multipartTimeoutFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return
	}
	if fixture.serveInitiate(w, r) {
		return
	}
	if fixture.serveStaged(w, r) {
		return
	}
	fixture.servePartOK(w, r)
}

func (fixture *multipartTimeoutFixture) serveInitiate(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost || !r.URL.Query().Has("uploads") {
		return false
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>fixture</Key><UploadId>fixture-upload</UploadId></InitiateMultipartUploadResult>`)
	return true
}

func (fixture *multipartTimeoutFixture) serveStaged(w http.ResponseWriter, r *http.Request) bool {
	if fixture.stage(r) == "" {
		return false
	}
	return fixture.stageRejects(w, r)
}

func (fixture *multipartTimeoutFixture) stage(r *http.Request) string {
	switch r.Method {
	case http.MethodPut:
		return "part"
	case http.MethodPost:
		return "completion"
	case http.MethodDelete:
		return "abort"
	}
	return ""
}

func (fixture *multipartTimeoutFixture) stageRejects(w http.ResponseWriter, r *http.Request) bool {
	stage := fixture.stage(r)
	if r.Method == http.MethodPut && fixture.phase == "abort" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<Error><Code>InvalidRequest</Code></Error>`)
		return true
	}
	if stage != fixture.phase {
		return false
	}
	fixture.entered <- struct{}{}
	select {
	case <-r.Context().Done():
	case <-fixture.release:
	}
	return true
}

func (fixture *multipartTimeoutFixture) servePartOK(w http.ResponseWriter, r *http.Request) {
	if fixture.stage(r) == "part" {
		w.Header().Set("ETag", `"fixture-part"`)
		fixture.mirrorChecksums(w, r)
	}
	w.WriteHeader(http.StatusOK)
}

func (fixture *multipartTimeoutFixture) mirrorChecksums(w http.ResponseWriter, r *http.Request) {
	for key, values := range r.Header {
		if strings.HasPrefix(strings.ToLower(key), "x-amz-checksum-") {
			w.Header()[key] = values
		}
	}
	for key, values := range r.Trailer {
		if strings.HasPrefix(strings.ToLower(key), "x-amz-checksum-") {
			w.Header()[key] = values
		}
	}
}

func (fixture *multipartTimeoutFixture) run(t *testing.T) {
	t.Helper()
	isolateS3AWS(t)
	httpServer := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer httpServer.Close()
	defer close(fixture.release)
	plugin, err := NewS3Target(map[string]any{"bucket": "bucket", "region": "us-east-1", "endpoint": httpServer.URL, "credentials_ref": `{"access_key_id":"x","secret_access_key":"y"}`, "operation_timeout": "2s", "max_message_bytes": 32 << 20}) // #nosec G101 -- fixture dummy credentials only
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := plugin.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	// Exceed the SDK's real default 16 MiB multipart threshold.
	msg := &plugins.Message{Folder: "INBOX", UID: "1", Raw: []byte(strings.Repeat("b", 17<<20))}
	done := make(chan error, 1)
	go func() { done <- plugin.WriteMessage(ctx, msg) }()
	fixture.waitEntered(t, done)
	fixture.waitDeadline(t, done)
}

func (fixture *multipartTimeoutFixture) waitEntered(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-fixture.entered:
	case err := <-done:
		t.Fatalf("multipart %s was not exercised: %v", fixture.phase, err)
	case <-time.After(4 * time.Second):
		t.Fatal("multipart request never arrived")
	}
}

func (fixture *multipartTimeoutFixture) waitDeadline(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("multipart deadline cause lost: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("multipart operation exceeded timeout")
	}
}

func TestS3MultipartTimeoutBoundsPartsCompletionAndAbort(t *testing.T) {
	for _, phase := range []string{"part", "completion", "abort"} {
		t.Run(phase, func(t *testing.T) {
			(&multipartTimeoutFixture{phase: phase, entered: make(chan struct{}, 4), release: make(chan struct{})}).run(t)
		})
	}
}
