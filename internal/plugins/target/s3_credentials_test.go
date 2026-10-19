package target

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestS3CredentialSelection(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(map[bool]string{true: "explicit", false: "default-chain"}[explicit], func(t *testing.T) {
			t.Setenv("AWS_ACCESS_KEY_ID", "ambient-key")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-test-value")
			t.Setenv("AWS_SESSION_TOKEN", "")
			t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
			t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
			t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
			type observed struct{ auth, token string }
			requests := make(chan observed, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- observed{r.Header.Get("Authorization"), r.Header.Get("X-Amz-Security-Token")}
			}))
			defer server.Close()
			values := map[string]interface{}{"bucket": "test-bucket", "region": "us-east-1", "endpoint": server.URL}
			wantKey, wantToken := "ambient-key", ""
			if explicit {
				values["credentials_ref"] = `{"access_key_id":"configured-key","secret_access_key":"configured-test-value","session_token":"configured-session"}`
				wantKey, wantToken = "configured-key", "configured-session"
			}
			target, err := NewS3Target(values)
			if err != nil {
				t.Fatal(err)
			}
			if err := target.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := <-requests
			if !strings.Contains(got.auth, "Credential="+wantKey+"/") || got.token != wantToken {
				t.Fatal("wrong credential provider used")
			}
		})
	}
}

func TestS3RejectsMalformedExplicitCredentials(t *testing.T) {
	for _, ref := range []string{"", "synthetic-sensitive-value", `{"access_key_id":"only-key"}`, `{"access_key_id":7,"secret_access_key":"synthetic-sensitive-value"}`} {
		_, err := NewS3Target(map[string]interface{}{"bucket": "test-bucket", "region": "us-east-1", "credentials_ref": ref})
		if err == nil {
			t.Fatal("malformed explicit credentials silently accepted")
		}
		if strings.Contains(err.Error(), "synthetic-sensitive-value") {
			t.Fatal("error leaked credential value")
		}
	}
}
