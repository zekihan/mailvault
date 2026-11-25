package target

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

const s3BackupBucket = "backup-fixture"
const s3BackupPrefix = "mail/"
const s3BackupOldKey = s3BackupPrefix + "original.eml"

type s3BackupListElement struct {
	Key string `xml:"Key"`
}

type s3BackupListResult struct {
	XMLName               xml.Name              `xml:"ListBucketResult"`
	Name                  string                `xml:"Name"`
	Prefix                string                `xml:"Prefix"`
	KeyCount              int                   `xml:"KeyCount"`
	MaxKeys               int                   `xml:"MaxKeys"`
	IsTruncated           bool                  `xml:"IsTruncated"`
	NextContinuationToken string                `xml:"NextContinuationToken,omitempty"`
	Contents              []s3BackupListElement `xml:"Contents"`
}

type s3BackupServer struct {
	t       *testing.T
	mu      sync.Mutex
	objects map[string][]byte
}

func (server *s3BackupServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead && r.URL.Path == "/"+s3BackupBucket {
		w.WriteHeader(http.StatusOK)
		return
	}
	if server.serveList(w, r) {
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/"+s3BackupBucket+"/")
	switch r.Method {
	case http.MethodGet:
		server.serveGet(w, key)
	case http.MethodPut:
		server.servePut(w, r, key)
	default:
		server.t.Errorf("unexpected S3 request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (server *s3BackupServer) serveList(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Query().Get("list-type") != "2" {
		return false
	}
	if r.URL.Query().Get("prefix") != s3BackupPrefix {
		server.t.Errorf("list prefix = %q", r.URL.Query().Get("prefix"))
	}
	result := s3BackupListResult{Name: s3BackupBucket, Prefix: s3BackupPrefix, MaxKeys: 1000}
	keys := server.sortedKeys()
	if r.URL.Query().Get("continuation-token") == "page-2" {
		server.appendOtherKeys(&result, keys)
	} else {
		server.appendOldKey(&result)
	}
	result.KeyCount = len(result.Contents)
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
	return true
}

func (server *s3BackupServer) sortedKeys() []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	keys := make([]string, 0, len(server.objects))
	for key := range server.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (server *s3BackupServer) appendOtherKeys(result *s3BackupListResult, keys []string) {
	for _, key := range keys {
		if key != s3BackupOldKey {
			result.Contents = append(result.Contents, s3BackupListElement{Key: key})
		}
	}
}

func (server *s3BackupServer) appendOldKey(result *s3BackupListResult) {
	result.Contents = append(result.Contents, s3BackupListElement{Key: s3BackupOldKey})
	result.IsTruncated = true
	result.NextContinuationToken = "page-2"
}

func (server *s3BackupServer) serveGet(w http.ResponseWriter, key string) {
	server.mu.Lock()
	body, ok := server.objects[key]
	server.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
		return
	}
	w.Header().Set("X-Amz-Meta-Source-Folder", "INBOX")
	w.Header().Set("X-Amz-Meta-Source-Uid", "42")
	_, _ = w.Write(body)
}

func (server *s3BackupServer) servePut(w http.ResponseWriter, r *http.Request, key string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		server.t.Errorf("read upload: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	server.mu.Lock()
	server.objects[key] = body
	server.mu.Unlock()
	w.Header().Set("ETag", `"fixture-etag"`)
	w.WriteHeader(http.StatusOK)
}

func openS3BackupServer(t *testing.T) (*s3BackupServer, *httptest.Server) {
	t.Helper()
	server := &s3BackupServer{t: t, objects: map[string][]byte{s3BackupOldKey: []byte("corrupt original")}}
	return server, httptest.NewServer(http.HandlerFunc(server.serve))
}

func openS3BackupTarget(t *testing.T, endpoint string) (*S3Target, error) {
	t.Helper()
	value, err := NewS3Target(map[string]any{ // #nosec G101 -- fixture dummy credentials only
		"bucket": s3BackupBucket, "region": "us-east-1", "endpoint": endpoint, "prefix": s3BackupPrefix,
		"credentials_ref": `{"access_key_id":"fixture","secret_access_key":"fixture"}`,
	})
	if err != nil {
		return nil, err
	}
	return mustAssert[*S3Target](t, value), nil
}

func checkS3BackupOriginal(t *testing.T, backup *S3Target) {
	t.Helper()
	ctx := t.Context()
	listed, err := backup.ListBackupMessages(ctx)
	if err != nil || len(listed) != 1 || listed[0].Locator != s3BackupOldKey {
		t.Fatalf("initial list = %+v, %v", listed, err)
	}
	old, err := backup.ReadBackupMessage(ctx, s3BackupOldKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(old.Raw) != "corrupt original" {
		t.Fatalf("read original = %q", old.Raw)
	}
	checkS3BackupInvalidReads(t, backup)
}

func checkS3BackupInvalidReads(t *testing.T, backup *S3Target) {
	t.Helper()
	ctx := t.Context()
	if _, err := backup.ReadBackupMessage(ctx, "outside/object.eml"); err == nil {
		t.Fatal("read accepted locator outside configured prefix")
	}
	if _, err := backup.ReadBackupMessage(ctx, s3BackupPrefix+"missing.eml"); !errors.Is(err, plugins.ErrBackupMessageMissing) {
		t.Fatalf("missing object error = %v", err)
	}
}

func checkS3BackupRepair(t *testing.T, backup *S3Target) string {
	t.Helper()
	ctx := t.Context()
	raw := []byte("Message-ID: <fixture@example.invalid>\r\n\r\nrepaired body\r\n")
	replacement, err := backup.WriteRepairMessage(ctx, &plugins.Message{Raw: raw, Folder: "INBOX", UID: "42"})
	if err != nil {
		t.Fatal(err)
	}
	if replacement == s3BackupOldKey || !strings.HasPrefix(replacement, s3BackupPrefix) {
		t.Fatalf("repair locator = %q, want a distinct key under %q", replacement, s3BackupPrefix)
	}
	readBack, err := backup.ReadBackupMessage(ctx, replacement)
	if err != nil || dedup.ComputeContentHash(readBack.Raw) != dedup.ComputeContentHash(raw) {
		t.Fatalf("read repaired object = %v, %v", readBack, err)
	}
	checkS3BackupListed(t, backup, replacement)
	return replacement
}

func checkS3BackupListed(t *testing.T, backup *S3Target, replacement string) {
	t.Helper()
	listed, err := backup.ListBackupMessages(t.Context())
	if err != nil || len(listed) != 2 {
		t.Fatalf("post-repair list = %+v, %v", listed, err)
	}
	locators := map[string]bool{}
	for i := range listed {
		locators[listed[i].Locator] = true
	}
	if !locators[s3BackupOldKey] || !locators[replacement] {
		t.Fatalf("listing omitted original or replacement: %+v", locators)
	}
}

func TestS3BackupReaderAndRepairRetainOriginalObject(t *testing.T) {
	isolateS3AWS(t)
	server, httpServer := openS3BackupServer(t)
	defer httpServer.Close()
	backup, err := openS3BackupTarget(t, httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkS3BackupOriginal(t, backup)
	replacement := checkS3BackupRepair(t, backup)
	checkS3BackupRetained(t, server, replacement, []byte("Message-ID: <fixture@example.invalid>\r\n\r\nrepaired body\r\n"))
}

func checkS3BackupRetained(t *testing.T, server *s3BackupServer, replacement string, raw []byte) {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	if string(server.objects[s3BackupOldKey]) != "corrupt original" || !bytes.Equal(server.objects[replacement], raw) {
		t.Fatal("repair did not retain the original object and publish exact replacement bytes")
	}
}

func TestS3ReadEnforcesActualStreamLimit(t *testing.T) {
	isolateS3AWS(t)
	const limit = 64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Flush first to omit Content-Length: the SDK cannot rely on advertised size.
		w.WriteHeader(http.StatusOK)
		mustAssert[http.Flusher](t, w).Flush()
		count := limit
		if strings.HasSuffix(r.URL.Path, "large.eml") {
			count++
		}
		_, _ = io.WriteString(w, strings.Repeat("x", count))
	}))
	defer server.Close()
	value, err := NewS3Target(map[string]any{"bucket": "fixture", "region": "us-east-1", "endpoint": server.URL, "max_message_bytes": limit, "credentials_ref": `{"access_key_id":"fixture","secret_access_key":"fixture"}`}) // #nosec G101 -- fixture dummy credentials only
	if err != nil {
		t.Fatal(err)
	}
	target := mustAssert[*S3Target](t, value)
	if err := target.InitializeForRead(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	msg, err := target.ReadBackupMessage(t.Context(), "exact.eml")
	if err != nil || len(msg.Raw) != limit {
		t.Fatalf("at-limit read: %v, %v", msg, err)
	}
	if _, err := target.ReadBackupMessage(t.Context(), "large.eml"); !errors.Is(err, plugins.ErrMessageTooLarge) {
		t.Fatalf("stream overflow accepted: %v", err)
	}
}
