package target

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalDestinationIdentityUsesCanonicalAbsolutePath(t *testing.T) {
	root := t.TempDir()
	first, err := NewMaildirTarget(map[string]interface{}{"path": filepath.Join(root, "mail")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewMaildirTarget(map[string]interface{}{"path": filepath.Join(root, "nested", "..", "mail")})
	if err != nil {
		t.Fatal(err)
	}
	if first.(interface{ DestinationIdentity() string }).DestinationIdentity() != second.(interface{ DestinationIdentity() string }).DestinationIdentity() {
		t.Fatal("equivalent local paths produced different destination identities")
	}
	other, err := NewMaildirTarget(map[string]interface{}{"path": filepath.Join(root, "other")})
	if err != nil {
		t.Fatal(err)
	}
	if first.(interface{ DestinationIdentity() string }).DestinationIdentity() == other.(interface{ DestinationIdentity() string }).DestinationIdentity() {
		t.Fatal("different local paths shared a destination identity")
	}
}

func TestLocalDestinationIdentityIsStableAcrossCreationAndSymlinkAliases(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0750); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(actual, alias); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	before, err := NewMaildirTarget(map[string]interface{}{"path": filepath.Join(alias, "mail")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(actual, "mail"), 0750); err != nil {
		t.Fatal(err)
	}
	after, err := NewMaildirTarget(map[string]interface{}{"path": filepath.Join(actual, "mail")})
	if err != nil {
		t.Fatal(err)
	}
	first := before.(interface{ DestinationIdentity() string }).DestinationIdentity()
	second := after.(interface{ DestinationIdentity() string }).DestinationIdentity()
	if first != second {
		t.Fatal("creating a destination or using a symlink ancestor changed its identity")
	}
}

func TestS3DestinationIdentityIncludesStorageCoordinatesWithoutCredentials(t *testing.T) {
	newTarget := func(bucket, region, endpoint, prefix string) *S3Target {
		target, err := NewS3Target(map[string]interface{}{"bucket": bucket, "region": region, "endpoint": endpoint, "prefix": prefix})
		if err != nil {
			t.Fatal(err)
		}
		return target.(*S3Target)
	}
	base := newTarget("archive", "us-east-1", "https://s3.example.invalid/", "mail")
	if base.DestinationIdentity() != newTarget("archive", "us-east-1", "https://s3.example.invalid", "mail/").DestinationIdentity() {
		t.Fatal("equivalent S3 endpoint and prefix forms produced different identities")
	}
	for _, changed := range []*S3Target{
		newTarget("other", "us-east-1", "https://s3.example.invalid", "mail/"),
		newTarget("archive", "us-west-2", "https://s3.example.invalid", "mail/"),
		newTarget("archive", "us-east-1", "https://other.example.invalid", "mail/"),
		newTarget("archive", "us-east-1", "https://s3.example.invalid", "other/"),
	} {
		if changed.DestinationIdentity() == base.DestinationIdentity() {
			t.Fatal("changed S3 storage coordinates shared a destination identity")
		}
	}
}
