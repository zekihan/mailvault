package plugins

import (
	"slices"
	"testing"
)

func TestNormalizeAppendFlags(t *testing.T) {
	got, err := NormalizeAppendFlags([]string{"\\recent", "\\SEEN", "\\Answered", "custom-keyword"})
	if err != nil {
		t.Fatalf("NormalizeAppendFlags() error: %v", err)
	}
	if want := []string{"\\Seen", "\\Answered", "custom-keyword"}; !slices.Equal(got, want) {
		t.Fatalf("NormalizeAppendFlags() = %v, want %v", got, want)
	}
	if got, err := NormalizeAppendFlags(nil); err != nil || len(got) != 0 {
		t.Fatalf("NormalizeAppendFlags(nil) = %v, %v; want empty", got, err)
	}
	for _, value := range []string{"", "\\Foo", "\\Seen\\Deleted", "has space", "bad(paren", "bad\\slash", "café"} {
		if _, err := NormalizeAppendFlags([]string{value}); err == nil {
			t.Fatalf("NormalizeAppendFlags(%q) accepted, want error", value)
		}
	}
}
