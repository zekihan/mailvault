package target

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

type metadataUpload struct {
	metadata map[string]string
	body     string
}

type metadataUploadServer struct {
	t       *testing.T
	uploads chan metadataUpload
}

func (server *metadataUploadServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	metadata, size := collectUploadMetadata(r)
	if size > 2048 {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<Error><Code>MetadataTooLarge</Code></Error>`)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		server.t.Errorf("read upload: %v", err)
		return
	}
	server.uploads <- metadataUpload{metadata, string(body)}
	w.Header().Set("ETag", `"fixture"`)
}

func collectUploadMetadata(r *http.Request) (map[string]string, int) {
	metadata := map[string]string{}
	size := 0
	for key, values := range r.Header {
		name := strings.TrimPrefix(strings.ToLower(key), "x-amz-meta-")
		if strings.EqualFold(name, key) {
			continue
		}
		for _, value := range values {
			metadata[name] = value
			size += len(name) + len(value)
		}
	}
	return metadata, size
}

type metadataCase struct {
	name, folder, uid, id string
	hashed                []string
}

func openMetadataTarget(t *testing.T, endpoint string) plugins.Target {
	t.Helper()
	plugin, err := NewS3Target(map[string]any{"bucket": "bucket", "region": "us-east-1", "endpoint": endpoint, "credentials_ref": `{"access_key_id":"x","secret_access_key":"y"}`}) // #nosec G101 -- fixture dummy credentials only
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	return plugin
}

func checkMetadataUpload(t *testing.T, plugin plugins.Target, uploads chan metadataUpload, tc *metadataCase) {
	t.Helper()
	raw := "Message-ID: <fixture@example.invalid>\r\n\r\nbody\r\n"
	msg := &plugins.Message{Folder: tc.folder, UID: tc.uid, MessageID: tc.id, Raw: []byte(raw), InternalDate: "2026-01-01T00:00:00Z", Size: int64(len(raw))}
	if err := plugin.WriteMessage(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	got := <-uploads
	if got.body != raw {
		t.Fatal("message bytes changed")
	}
	checkMetadataFields(t, got.metadata, tc, msg, raw)
}

func checkMetadataFields(t *testing.T, metadata map[string]string, tc *metadataCase, msg *plugins.Message, raw string) {
	t.Helper()
	original := map[string]string{"source-folder": tc.folder, "source-uid": tc.uid, "message-id": tc.id}
	checkHashedMetadata(t, metadata, original, tc.hashed)
	checkPlainMetadata(t, metadata, original, msg, raw)
}

func checkHashedMetadata(t *testing.T, metadata, original map[string]string, hashed []string) {
	t.Helper()
	for _, key := range hashed {
		if _, ok := metadata[key]; ok {
			t.Fatalf("oversized %s retained", key)
		}
		if want := fmt.Sprintf("%x", sha256.Sum256([]byte(original[key]))); metadata[key+"-sha256"] != want {
			t.Fatalf("missing digest for %s", key)
		}
		delete(original, key)
	}
}

func checkPlainMetadata(t *testing.T, metadata, original map[string]string, msg *plugins.Message, raw string) {
	t.Helper()
	for key, want := range original {
		if metadata[key] != want {
			t.Fatalf("ordinary %s changed", key)
		}
	}
	if metadata["content-hash"] != fmt.Sprintf("%x", sha256.Sum256([]byte(raw))) {
		t.Fatal("content digest changed")
	}
	if metadata["internal-date"] != msg.InternalDate || metadata["size"] != strconv.Itoa(len(raw)) {
		t.Fatal("ordinary metadata changed")
	}
}

func TestS3BoundsMetadataWithoutChangingMessage(t *testing.T) {
	isolateS3AWS(t)
	server := &metadataUploadServer{t: t, uploads: make(chan metadataUpload, 4)}
	httpServer := httptest.NewServer(http.HandlerFunc(server.serve))
	defer httpServer.Close()
	plugin := openMetadataTarget(t, httpServer.URL)
	cases := []metadataCase{
		{name: "ordinary", folder: "INBOX", uid: "42", id: "<fixture@example.invalid>"},
		{name: "unicode-folder", folder: strings.Repeat("界/", 600), uid: "42", id: "<fixture@example.invalid>", hashed: []string{"source-folder"}}, //nolint:gosmopolitan // Unicode (Han script) folder fixture; the byte-limit checks require multibyte runes
		{name: "multiple-large-fields", folder: strings.Repeat("folder/", 500), uid: strings.Repeat("u", 3000), id: strings.Repeat("m", 3000), hashed: []string{"source-folder", "source-uid", "message-id"}},
	}
	for i := range cases {
		t.Run(cases[i].name, func(t *testing.T) {
			checkMetadataUpload(t, plugin, server.uploads, &cases[i])
		})
	}
}
