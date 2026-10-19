package target

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestS3BoundsMetadataWithoutChangingMessage(t *testing.T) {
	isolateS3AWS(t)
	type upload struct {
		metadata map[string]string
		body     string
	}
	uploads := make(chan upload, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(200)
			return
		}
		metadata := map[string]string{}
		size := 0
		for key, values := range r.Header {
			name := strings.TrimPrefix(strings.ToLower(key), "x-amz-meta-")
			if name == strings.ToLower(key) {
				continue
			}
			for _, value := range values {
				metadata[name] = value
				size += len(name) + len(value)
			}
		}
		if size > 2048 {
			w.WriteHeader(400)
			fmt.Fprint(w, `<Error><Code>MetadataTooLarge</Code></Error>`)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upload: %v", err)
			return
		}
		uploads <- upload{metadata, string(body)}
		w.Header().Set("ETag", `"fixture"`)
	}))
	defer server.Close()
	plugin, err := NewS3Target(map[string]interface{}{"bucket": "bucket", "region": "us-east-1", "endpoint": server.URL, "credentials_ref": `{"access_key_id":"x","secret_access_key":"y"}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw := "Message-ID: <fixture@example.invalid>\r\n\r\nbody\r\n"
	for _, tc := range []struct {
		name, folder, uid, id string
		hashed                []string
	}{
		{"ordinary", "INBOX", "42", "<fixture@example.invalid>", nil},
		{"unicode-folder", strings.Repeat("界/", 600), "42", "<fixture@example.invalid>", []string{"source-folder"}},
		{"multiple-large-fields", strings.Repeat("folder/", 500), strings.Repeat("u", 3000), strings.Repeat("m", 3000), []string{"source-folder", "source-uid", "message-id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := &plugins.Message{Folder: tc.folder, UID: tc.uid, MessageID: tc.id, Raw: []byte(raw), InternalDate: "2026-01-01T00:00:00Z", Size: int64(len(raw))}
			if err := plugin.WriteMessage(context.Background(), msg); err != nil {
				t.Fatal(err)
			}
			got := <-uploads
			if got.body != raw {
				t.Fatal("message bytes changed")
			}
			original := map[string]string{"source-folder": tc.folder, "source-uid": tc.uid, "message-id": tc.id}
			for _, key := range tc.hashed {
				if _, ok := got.metadata[key]; ok {
					t.Fatalf("oversized %s retained", key)
				}
				if want := fmt.Sprintf("%x", sha256.Sum256([]byte(original[key]))); got.metadata[key+"-sha256"] != want {
					t.Fatalf("missing digest for %s", key)
				}
				delete(original, key)
			}
			for key, want := range original {
				if got.metadata[key] != want {
					t.Fatalf("ordinary %s changed", key)
				}
			}
			if got.metadata["content-hash"] != fmt.Sprintf("%x", sha256.Sum256([]byte(raw))) {
				t.Fatal("content digest changed")
			}
			if got.metadata["internal-date"] != msg.InternalDate || got.metadata["size"] != fmt.Sprint(len(raw)) {
				t.Fatal("ordinary metadata changed")
			}
		})
	}
}
