package config

import (
	"math"
	"strconv"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestParseInt(t *testing.T) {
	if got, err := ParseInt(map[string]interface{}{}, "n", 7); err != nil || got != 7 {
		t.Fatalf("missing key = %d, %v; want default 7", got, err)
	}
	tests := []struct {
		name    string
		value   interface{}
		want    int
		wantErr bool
	}{
		{"int", int(3), 3, false},
		{"int64", int64(-4), -4, false},
		{"integral float", float64(5), 5, false},
		{"fraction", 1.9, 0, true},
		{"negative fraction", -1.9, 0, true},
		{"string", "3", 0, true},
		{"above int range", math.Ldexp(1, strconv.IntSize-1), 0, true},
		{"below int range", -math.Ldexp(1, strconv.IntSize), 0, true},
		{"minimum int float", -math.Ldexp(1, strconv.IntSize-1), -int(^uint(0)>>1) - 1, false},
		{"nan", math.NaN(), 0, true},
		{"infinity", math.Inf(1), 0, true},
		{"bool", true, 0, true},
		{"nil", nil, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseInt(map[string]interface{}{"n": tt.value}, "n", 0)
			if (err != nil) != tt.wantErr || (!tt.wantErr && got != tt.want) {
				t.Fatalf("ParseInt(%v) = %d, %v; want %d, error=%t", tt.value, got, err, tt.want, tt.wantErr)
			}
		})
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
