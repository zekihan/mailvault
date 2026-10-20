package target

import (
	"context"
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

func TestS3BackupReaderAndRepairRetainOriginalObject(t *testing.T) {
	isolateS3AWS(t)
	const bucket = "backup-fixture"
	const prefix = "mail/"
	const oldKey = prefix + "original.eml"
	var mu sync.Mutex
	objects := map[string][]byte{oldKey: []byte("corrupt original")}
	type listResult struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		Name                  string   `xml:"Name"`
		Prefix                string   `xml:"Prefix"`
		KeyCount              int      `xml:"KeyCount"`
		MaxKeys               int      `xml:"MaxKeys"`
		IsTruncated           bool
		NextContinuationToken string `xml:"NextContinuationToken,omitempty"`
		Contents              []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/"+bucket {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
			if r.URL.Query().Get("prefix") != prefix {
				t.Errorf("list prefix = %q", r.URL.Query().Get("prefix"))
			}
			result := listResult{Name: bucket, Prefix: prefix, MaxKeys: 1000}
			mu.Lock()
			keys := make([]string, 0, len(objects))
			for key := range objects {
				keys = append(keys, key)
			}
			mu.Unlock()
			sort.Strings(keys)
			if r.URL.Query().Get("continuation-token") == "page-2" {
				for _, key := range keys {
					if key != oldKey {
						result.Contents = append(result.Contents, struct {
							Key string `xml:"Key"`
						}{Key: key})
					}
				}
			} else {
				result.Contents = append(result.Contents, struct {
					Key string `xml:"Key"`
				}{Key: oldKey})
				result.IsTruncated = true
				result.NextContinuationToken = "page-2"
			}
			result.KeyCount = len(result.Contents)
			w.Header().Set("Content-Type", "application/xml")
			_ = xml.NewEncoder(w).Encode(result)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/"+bucket+"/")
		switch r.Method {
		case http.MethodGet:
			mu.Lock()
			body, ok := objects[key]
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
				return
			}
			w.Header().Set("X-Amz-Meta-Source-Folder", "INBOX")
			w.Header().Set("X-Amz-Meta-Source-Uid", "42")
			_, _ = w.Write(body)
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upload: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			mu.Lock()
			objects[key] = body
			mu.Unlock()
			w.Header().Set("ETag", `"fixture-etag"`)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected S3 request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	value, err := NewS3Target(map[string]interface{}{
		"bucket": bucket, "region": "us-east-1", "endpoint": server.URL, "prefix": prefix,
		"credentials_ref": `{"access_key_id":"fixture","secret_access_key":"fixture"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	backup := value.(*S3Target)
	ctx := context.Background()
	if err := backup.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	listed, err := backup.ListBackupMessages(ctx)
	if err != nil || len(listed) != 1 || listed[0].Locator != oldKey {
		t.Fatalf("initial list = %+v, %v", listed, err)
	}
	old, err := backup.ReadBackupMessage(ctx, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(old.Raw) != "corrupt original" {
		t.Fatalf("read original = %q", old.Raw)
	}
	if _, err := backup.ReadBackupMessage(ctx, "outside/object.eml"); err == nil {
		t.Fatal("read accepted locator outside configured prefix")
	}
	if _, err := backup.ReadBackupMessage(ctx, prefix+"missing.eml"); !errors.Is(err, plugins.ErrBackupMessageMissing) {
		t.Fatalf("missing object error = %v", err)
	}

	raw := []byte("Message-ID: <fixture@example.invalid>\r\n\r\nrepaired body\r\n")
	replacement, err := backup.WriteRepairMessage(ctx, &plugins.Message{Raw: raw, Folder: "INBOX", UID: "42"})
	if err != nil {
		t.Fatal(err)
	}
	if replacement == oldKey || !strings.HasPrefix(replacement, prefix) {
		t.Fatalf("repair locator = %q, want a distinct key under %q", replacement, prefix)
	}
	readBack, err := backup.ReadBackupMessage(ctx, replacement)
	if err != nil || dedup.ComputeContentHash(readBack.Raw) != dedup.ComputeContentHash(raw) {
		t.Fatalf("read repaired object = %v, %v", readBack, err)
	}
	listed, err = backup.ListBackupMessages(ctx)
	if err != nil || len(listed) != 2 {
		t.Fatalf("post-repair list = %+v, %v", listed, err)
	}
	listedLocators := map[string]bool{}
	for _, entry := range listed {
		listedLocators[entry.Locator] = true
	}
	if !listedLocators[oldKey] || !listedLocators[replacement] {
		t.Fatalf("listing omitted original or replacement: %+v", listedLocators)
	}
	mu.Lock()
	defer mu.Unlock()
	if string(objects[oldKey]) != "corrupt original" || string(objects[replacement]) != string(raw) {
		t.Fatal("repair did not retain the original object and publish exact replacement bytes")
	}
}
