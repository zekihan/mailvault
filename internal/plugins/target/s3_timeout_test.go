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

func TestS3OperationTimeoutBoundsStalledRequests(t *testing.T) {
	for _, phase := range []string{"initialize", "upload", "response-body"} {
		t.Run(phase, func(t *testing.T) {
			isolateS3AWS(t)
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase != "initialize" && r.Method == http.MethodHead {
					w.WriteHeader(200)
					return
				}
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					return
				}
				entered <- struct{}{}
				if phase == "response-body" {
					w.Header().Set("Content-Length", "1000")
					w.WriteHeader(500)
					fmt.Fprint(w, "<Error>")
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			plugin, err := NewS3Target(map[string]interface{}{"bucket": "bucket", "region": "us-east-1", "endpoint": server.URL, "credentials_ref": `{"access_key_id":"x","secret_access_key":"y"}`, "operation_timeout": "100ms"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase != "initialize" {
				if err := plugin.Initialize(ctx); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() {
				if phase == "initialize" {
					done <- plugin.Initialize(ctx)
				} else {
					done <- plugin.WriteMessage(ctx, &plugins.Message{Folder: "INBOX", UID: "1", Raw: []byte("Subject: fixture\r\n\r\nbody\r\n")})
				}
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("request did not reach fixture: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("request did not start")
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("wanted deadline error: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("stalled S3 operation did not time out")
			}
		})
	}
}

func TestS3RejectsInvalidOperationTimeout(t *testing.T) {
	for _, value := range []interface{}{"0s", "-1s", "invalid", 0, true} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			_, err := NewS3Target(map[string]interface{}{"bucket": "bucket", "region": "us-east-1", "operation_timeout": value})
			if err == nil || !strings.Contains(err.Error(), "operation_timeout") {
				t.Fatalf("invalid timeout accepted or wrong error: %v", err)
			}
		})
	}
}
