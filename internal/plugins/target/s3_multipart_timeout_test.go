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

func TestS3MultipartTimeoutBoundsPartsCompletionAndAbort(t *testing.T) {
	for _, phase := range []string{"part", "completion", "abort"} {
		t.Run(phase, func(t *testing.T) {
			isolateS3AWS(t)
			entered := make(chan struct{}, 4)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(200)
					return
				}
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					return
				}
				query := r.URL.Query()
				if r.Method == http.MethodPost && query.Has("uploads") {
					w.Header().Set("Content-Type", "application/xml")
					fmt.Fprint(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>fixture</Key><UploadId>fixture-upload</UploadId></InitiateMultipartUploadResult>`)
					return
				}
				stage := ""
				switch r.Method {
				case http.MethodPut:
					stage = "part"
					if phase == "abort" {
						w.WriteHeader(400)
						fmt.Fprint(w, `<Error><Code>InvalidRequest</Code></Error>`)
						return
					}
				case http.MethodPost:
					stage = "completion"
				case http.MethodDelete:
					stage = "abort"
				}
				if stage == phase {
					entered <- struct{}{}
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				if stage == "part" {
					w.Header().Set("ETag", `"fixture-part"`)
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
				w.WriteHeader(200)
			}))
			defer server.Close()
			defer close(release)
			plugin, err := NewS3Target(map[string]interface{}{"bucket": "bucket", "region": "us-east-1", "endpoint": server.URL, "credentials_ref": `{"access_key_id":"x","secret_access_key":"y"}`, "operation_timeout": "2s"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := plugin.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			// Exceed the SDK's real default 16 MiB multipart threshold.
			msg := &plugins.Message{Folder: "INBOX", UID: "1", Raw: []byte(strings.Repeat("b", 17<<20))}
			done := make(chan error, 1)
			go func() { done <- plugin.WriteMessage(ctx, msg) }()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("multipart %s was not exercised: %v", phase, err)
			case <-time.After(4 * time.Second):
				t.Fatal("multipart request never arrived")
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("multipart deadline cause lost: %v", err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("multipart operation exceeded timeout")
			}
		})
	}
}
