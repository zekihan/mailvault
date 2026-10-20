package source

import (
	"reflect"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestAppendFlagsPreserveSafeFlagsAndExcludeRecent(t *testing.T) {
	flags, err := appendFlags([]string{"\\Seen", "\\Recent", "$Label1", "Project-Blue"})
	if err != nil {
		t.Fatal(err)
	}
	want := []imap.Flag{imap.FlagSeen, "$Label1", "Project-Blue"}
	if !reflect.DeepEqual(flags, want) {
		t.Fatalf("flags = %#v, want %#v", flags, want)
	}
}

func TestAppendFlagsRejectUnsafeAndUnknownSystemFlags(t *testing.T) {
	for _, flag := range []string{"\\Unknown", "bad keyword", "x) LOGIN"} {
		if _, err := appendFlags([]string{flag}); err == nil {
			t.Errorf("accepted unsafe flag %q", flag)
		}
	}
}
