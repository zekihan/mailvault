package target

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestS3OperationTimeoutDefaultAndCallerDeadline(t *testing.T) {
	plugin, err := NewS3Target(map[string]interface{}{"bucket": "bucket", "region": "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	target := plugin.(*S3Target)
	if target.operationTimeout != 5*time.Minute {
		t.Fatalf("default timeout=%s", target.operationTimeout)
	}
	ctx, cancel := target.operationContext(context.Background())
	defer cancel()
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Minute {
		t.Fatal("default operation has no valid finite deadline")
	}
	parent, parentCancel := context.WithTimeout(context.Background(), time.Second)
	defer parentCancel()
	child, childCancel := target.operationContext(parent)
	defer childCancel()
	want, _ := parent.Deadline()
	got, ok := child.Deadline()
	if !ok || !got.Equal(want) {
		t.Fatal("earlier caller deadline was extended")
	}
	parentCancel()
	if child.Err() != context.Canceled {
		t.Fatalf("caller cancellation lost: %v", child.Err())
	}
}

func TestS3MetadataBudgetBoundary(t *testing.T) {
	msg := &plugins.Message{}
	base := s3ObjectMetadata(msg, [32]byte{})
	overhead := 0
	for key, value := range base {
		overhead += len(key) + len(value)
	}
	for _, size := range []int{2048, 2049} {
		msg.Folder = strings.Repeat("f", size-overhead)
		got := s3ObjectMetadata(msg, [32]byte{})
		actual := 0
		for key, value := range got {
			actual += len(key) + len(value)
		}
		if actual > 2048 {
			t.Fatalf("metadata bytes=%d", actual)
		}
		if size == 2048 && got["source-folder"] != msg.Folder {
			t.Fatal("exact-budget metadata was changed")
		}
		if size == 2049 && got["source-folder-sha256"] == "" {
			t.Fatal("over-budget metadata was not replaced with explicit digest")
		}
	}
}
