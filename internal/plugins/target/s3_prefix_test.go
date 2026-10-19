package target

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func TestS3BoundedIdentityIsStableAndDistinct(t *testing.T) {
	content := sha256.Sum256([]byte("fixture"))
	folder := strings.Repeat("界", 200)
	key := s3ObjectKey("archive/", content, folder, "42")
	if key != s3ObjectKey("archive/", content, folder, "42") {
		t.Fatal("bounded identity is not retry-stable")
	}
	if key == s3ObjectKey("archive/", content, folder+"other", "42") || key == s3ObjectKey("archive/", content, folder, "43") {
		t.Fatal("bounded identity lost folder/UID distinctions")
	}
	if len(key) > maxS3ObjectKeyBytes {
		t.Fatal("bounded key exceeds S3 limit")
	}
}

func TestS3PrefixReservesBoundedObjectName(t *testing.T) {
	const maxPrefix = 1024 - (sha256.Size*4 + len("-.eml"))
	for _, tc := range []struct {
		name, prefix string
		valid        bool
	}{
		{"boundary", strings.Repeat("p", maxPrefix-1), true},
		{"too-long-after-slash", strings.Repeat("p", maxPrefix), false},
		{"unicode-byte-limit", strings.Repeat("界", maxPrefix/3+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := NewS3Target(map[string]interface{}{"bucket": "fixture", "region": "us-east-1", "prefix": tc.prefix})
			if !tc.valid {
				if err == nil {
					t.Fatal("prefix cannot fit bounded object name but was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			actual := target.(*S3Target)
			key := s3ObjectKey(actual.prefix, sha256.Sum256([]byte("fixture")), strings.Repeat("界", 200), "42")
			if len(key) > 1024 {
				t.Fatalf("boundary prefix still produced %d-byte key", len(key))
			}
		})
	}
}
