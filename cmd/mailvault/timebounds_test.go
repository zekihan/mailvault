package main

import (
	"testing"
	"time"
)

func TestTimestampBoundsPreserveSecondResolutionComparisons(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"2024-01-01T12:00:00Z", "2024-01-01T12:00:00Z"},
		{"2024-01-01T12:00:00.5Z", "2024-01-01T12:00:01Z"},
		{"2024-01-01T12:00:00.000000001Z", "2024-01-01T12:00:01Z"},
		{"2024-01-01T15:00:00.5+03:00", "2024-01-01T12:00:01Z"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseTimeBound(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.Parse(time.RFC3339, tc.want)
			if err != nil {
				t.Fatal(err)
			}
			if got != want.Unix() {
				t.Fatalf("timestamp bound was truncated: got %s want %s", time.Unix(got, 0).UTC(), want)
			}
		})
	}
	if got, err := parseTimeBound(""); err != nil || got != 0 {
		t.Fatalf("empty bound: %d %v", got, err)
	}
	for _, input := range []string{"invalid", "1970-01-01T00:00:00Z", "1969-12-31T23:59:59.5Z"} {
		if _, err := parseTimeBound(input); err == nil {
			t.Fatalf("unrepresentable or invalid bound %q accepted", input)
		}
	}
}
