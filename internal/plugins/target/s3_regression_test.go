package target

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func isolateS3AWS(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "absent-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "absent-credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_DEFAULT_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "synthetic-id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "synthetic-secret")
}

func TestS3RejectsMalformedStringOptions(t *testing.T) {
	for _, field := range []string{"bucket", "region", "endpoint", "prefix"} {
		t.Run(field, func(t *testing.T) {
			values := map[string]interface{}{"bucket": "bucket", "region": "us-east-1", "endpoint": "http://127.0.0.1:1", "prefix": "private/"}
			values[field] = 23
			_, err := NewS3Target(values)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), field) {
				t.Fatalf("error = %v, want field %q", err, field)
			}
			if strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestS3BoundsLongUnicodeObjectKeys(t *testing.T) {
	isolateS3AWS(t)
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(200)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")
		if len([]byte(key)) > 1024 {
			w.WriteHeader(400)
			_, _ = fmt.Fprint(w, `<Error><Code>KeyTooLongError</Code></Error>`)
			return
		}
		keys = append(keys, key)
		w.WriteHeader(200)
	}))
	defer server.Close()
	p, err := NewS3Target(map[string]interface{}{"bucket": "bucket", "region": "us-east-1", "endpoint": server.URL, "credentials_ref": `{"access_key_id":"x","secret_access_key":"y"}`})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw := []byte("body")
	for _, m := range []*plugins.Message{{Folder: strings.Repeat("界", 200), UID: "42", Raw: raw}, {Folder: "INBOX", UID: "42", Raw: raw}, {Folder: "Archive", UID: "42", Raw: raw}, {Folder: "INBOX", UID: "43", Raw: raw}} {
		if err = p.WriteMessage(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 4 {
		t.Fatalf("uploaded keys = %d", len(keys))
	}
	if keys[1] == keys[2] || keys[1] == keys[3] {
		t.Fatalf("keys not distinct: %q", keys)
	}
	if len(keys[0]) > 1024 {
		t.Fatalf("long key bytes=%d", len(keys[0]))
	}
}

func TestS3RejectsPrefixThatCannotFitKey(t *testing.T) {
	_, err := NewS3Target(map[string]interface{}{"bucket": "bucket", "region": "us-east-1", "prefix": strings.Repeat("p", 1025)})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "prefix") {
		t.Fatalf("error = %v", err)
	}
}

func TestS3AcceptsValidStringOptions(t *testing.T) {
	p, err := NewS3Target(map[string]interface{}{"bucket": "bucket", "region": "us-east-1", "endpoint": "http://localhost:9000", "prefix": "private"})
	if err != nil {
		t.Fatal(err)
	}
	target := p.(*S3Target)
	if target.bucket != "bucket" || target.region != "us-east-1" || target.endpoint != "http://localhost:9000" || target.prefix != "private/" {
		t.Fatalf("target options = %#v", target)
	}
}
