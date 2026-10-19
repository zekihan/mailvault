package target

import (
	"math"
	"testing"
	"time"
)

func TestS3NumericOperationTimeoutRange(t *testing.T) {
	for _, value := range []interface{}{int64(18446744074), int64(-9223372037), float64(18446744074), math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := NewS3Target(map[string]interface{}{"bucket": "bucket", "region": "us-east-1", "operation_timeout": value}); err == nil {
			t.Fatalf("invalid numeric timeout accepted: %v", value)
		}
	}
	plugin, err := NewS3Target(map[string]interface{}{"bucket": "bucket", "region": "us-east-1", "operation_timeout": 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if got := plugin.(*S3Target).operationTimeout; got != 500*time.Millisecond {
		t.Fatalf("fractional seconds parsed as %s", got)
	}
}
