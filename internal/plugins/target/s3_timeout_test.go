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

type stalledS3Fixture struct {
	t       *testing.T
	phase   string
	entered chan struct{}
	release chan struct{}
}

func (fixture *stalledS3Fixture) serve(w http.ResponseWriter, r *http.Request) {
	if fixture.phase != "initialize" && r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return
	}
	fixture.entered <- struct{}{}
	if fixture.phase == "response-body" {
		fixture.writeStalledBody(w)
	}
	select {
	case <-r.Context().Done():
	case <-fixture.release:
	}
}

func (fixture *stalledS3Fixture) writeStalledBody(w http.ResponseWriter) {
	w.Header().Set("Content-Length", "1000")
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprint(w, "<Error>")
	mustAssert[http.Flusher](fixture.t, w).Flush()
}

func (fixture *stalledS3Fixture) run(t *testing.T) {
	t.Helper()
	isolateS3AWS(t)
	fixture.t = t
	httpServer := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer httpServer.Close()
	defer close(fixture.release)
	plugin, err := NewS3Target(map[string]any{"bucket": "bucket", "region": "us-east-1", "endpoint": httpServer.URL, "credentials_ref": `{"access_key_id":"x","secret_access_key":"y"}`, "operation_timeout": "100ms"}) // #nosec G101 -- fixture dummy credentials only
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if fixture.phase != "initialize" {
		if err := plugin.Initialize(ctx); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- fixture.stalledOperation(ctx, plugin) }()
	fixture.waitEntered(t, done)
	fixture.waitDeadline(t, done)
}

func (fixture *stalledS3Fixture) stalledOperation(ctx context.Context, plugin interface {
	Initialize(ctx context.Context) error
	WriteMessage(ctx context.Context, msg *plugins.Message) error
}) error {
	if fixture.phase == "initialize" {
		return plugin.Initialize(ctx)
	}
	return plugin.WriteMessage(ctx, &plugins.Message{Folder: "INBOX", UID: "1", Raw: []byte("Subject: fixture\r\n\r\nbody\r\n")})
}

func (fixture *stalledS3Fixture) waitEntered(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-fixture.entered:
	case err := <-done:
		t.Fatalf("request did not reach fixture: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
}

func (fixture *stalledS3Fixture) waitDeadline(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wanted deadline error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled S3 operation did not time out")
	}
}

func TestS3OperationTimeoutBoundsStalledRequests(t *testing.T) {
	for _, phase := range []string{"initialize", "upload", "response-body"} {
		t.Run(phase, func(t *testing.T) {
			(&stalledS3Fixture{phase: phase, entered: make(chan struct{}, 1), release: make(chan struct{})}).run(t)
		})
	}
}

func TestS3RejectsInvalidOperationTimeout(t *testing.T) {
	for _, value := range []any{"0s", "-1s", "invalid", 0, true} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			_, err := NewS3Target(map[string]any{"bucket": "bucket", "region": "us-east-1", "operation_timeout": value})
			if err == nil || !strings.Contains(err.Error(), "operation_timeout") {
				t.Fatalf("invalid timeout accepted or wrong error: %v", err)
			}
		})
	}
}
