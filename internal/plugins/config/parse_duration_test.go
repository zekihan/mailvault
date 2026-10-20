package config

import (
	"math"
	"strconv"
	"testing"
	"time"
)

func TestParseDurationNumericRange(t *testing.T) {
	maxSeconds := int64(math.MaxInt64 / int64(time.Second))
	minSeconds := int64(math.MinInt64 / int64(time.Second))
	tests := []struct {
		name    string
		value   interface{}
		want    time.Duration
		wantErr bool
	}{
		{"int seconds", int(2), 2 * time.Second, false},
		{"int64 max seconds", maxSeconds, time.Duration(maxSeconds) * time.Second, false},
		{"int64 overflow seconds", maxSeconds + 1, 0, true},
		{"int64 min boundary", minSeconds, time.Duration(minSeconds) * time.Second, false},
		{"int64 underflow seconds", minSeconds - 1, 0, true},
		{"typed max duration", time.Duration(math.MaxInt64), time.Duration(math.MaxInt64), false},
		{"typed min duration", time.Duration(math.MinInt64), time.Duration(math.MinInt64), false},
		{"string fraction", "1.5s", 1500 * time.Millisecond, false},
		{"int64 wrap positive", int64(18446744074), 0, true},
		{"int64 underflow", int64(-9223372037), 0, true},
		{"float NaN", math.NaN(), 0, true},
		{"float positive infinity", math.Inf(1), 0, true},
		{"float negative infinity", math.Inf(-1), 0, true},
		{"float positive overflow", float64(9223372037), 0, true},
		{"float negative overflow", float64(-9223372037), 0, true},
		{"float fraction seconds", 1.5, 1500 * time.Millisecond, false},
	}
	if strconv.IntSize == 64 {
		for _, seconds := range []int64{maxSeconds, maxSeconds + 1, minSeconds, minSeconds - 1} {
			tests = append(tests, struct {
				name    string
				value   interface{}
				want    time.Duration
				wantErr bool
			}{"int " + strconv.FormatInt(seconds, 10), int(seconds), time.Duration(seconds) * time.Second, seconds > maxSeconds || seconds < minSeconds})
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDuration(map[string]interface{}{"d": tt.value}, "d", 0)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseDuration() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("ParseDuration() = %v, want %v", got, tt.want)
			}
		})
	}
}
