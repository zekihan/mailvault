package config

import (
	"math"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestParseInt(t *testing.T) {
	if got, err := ParseInt(map[string]interface{}{}, "n", 7); err != nil || got != 7 {
		t.Fatalf("missing key = %d, %v; want default 7", got, err)
	}
	for value, want := range map[interface{}]int{int(3): 3, int64(-4): -4, float64(5): 5} {
		if got, err := ParseInt(map[string]interface{}{"n": value}, "n", 0); err != nil || got != want {
			t.Fatalf("ParseInt(%T %v) = %d, %v; want %d", value, value, got, err, want)
		}
	}
	for _, value := range []interface{}{"3", true, nil} {
		if _, err := ParseInt(map[string]interface{}{"n": value}, "n", 0); err == nil {
			t.Fatalf("ParseInt(%T %v) accepted, want error", value, value)
		}
	}
}

func TestParseStringAndBool(t *testing.T) {
	if got, err := ParseString(map[string]interface{}{}, "s", "fallback"); err != nil || got != "fallback" {
		t.Fatalf("missing string = %q, %v; want fallback", got, err)
	}
	if got, err := ParseString(map[string]interface{}{"s": "value"}, "s", ""); err != nil || got != "value" {
		t.Fatalf("string = %q, %v; want value", got, err)
	}
	if _, err := ParseString(map[string]interface{}{"s": 1}, "s", ""); err == nil {
		t.Fatal("non-string value accepted")
	}
	if got, err := ParseBool(map[string]interface{}{}, "b", true); err != nil || !got {
		t.Fatalf("missing bool = %v, %v; want default true", got, err)
	}
	if got, err := ParseBool(map[string]interface{}{"b": false}, "b", true); err != nil || got {
		t.Fatalf("bool = %v, %v; want false", got, err)
	}
	if _, err := ParseBool(map[string]interface{}{"b": "true"}, "b", false); err == nil {
		t.Fatal("string value accepted as bool")
	}
}

func TestParseStringSlice(t *testing.T) {
	if got, err := ParseStringSlice(map[string]interface{}{}, "s"); err != nil || got != nil {
		t.Fatalf("missing slice = %v, %v; want nil", got, err)
	}
	if got, err := ParseStringSlice(map[string]interface{}{"s": []interface{}{"a", "b"}}, "s"); err != nil || len(got) != 2 || got[1] != "b" {
		t.Fatalf("interface slice = %v, %v; want [a b]", got, err)
	}
	if got, err := ParseStringSlice(map[string]interface{}{"s": []string{"a"}}, "s"); err != nil || len(got) != 1 {
		t.Fatalf("string slice = %v, %v; want [a]", got, err)
	}
	for _, value := range []interface{}{[]interface{}{"a", 1}, "a", 1} {
		if _, err := ParseStringSlice(map[string]interface{}{"s": value}, "s"); err == nil {
			t.Fatalf("ParseStringSlice(%T %v) accepted, want error", value, value)
		}
	}
}

func TestParseMessageLimit(t *testing.T) {
	if got, err := ParseMessageLimit(map[string]interface{}{}); err != nil || got != plugins.DefaultMaxMessageBytes {
		t.Fatalf("missing limit = %d, %v; want default %d", got, err, plugins.DefaultMaxMessageBytes)
	}
	for value, want := range map[interface{}]int64{
		int(1024): 1024, int64(2048): 2048, float64(4096): 4096, "8192": 8192,
		plugins.MaxMessageBytes: plugins.MaxMessageBytes,
	} {
		if got, err := ParseMessageLimit(map[string]interface{}{"max_message_bytes": value}); err != nil || got != want {
			t.Fatalf("ParseMessageLimit(%T %v) = %d, %v; want %d", value, value, got, err, want)
		}
	}
	for _, value := range []interface{}{
		0, -1, plugins.MaxMessageBytes + 1, 1.5, math.NaN(), math.Inf(1),
		"invalid", "1.5", true, nil,
	} {
		if _, err := ParseMessageLimit(map[string]interface{}{"max_message_bytes": value}); err == nil {
			t.Fatalf("ParseMessageLimit(%T %v) accepted, want error", value, value)
		}
	}
}
