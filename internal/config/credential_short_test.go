package config

import "testing"

func TestResolveCredentialRefShortKeyringPrefixesDoNotPanic(t *testing.T) {
	for _, ref := range []string{"keyri", "keyrin", "keyring"} {
		got, err := resolveCredentialRef(ref)
		if err != nil || got != ref {
			t.Fatalf("resolve(%q) = %q, %v", ref, got, err)
		}
	}
}
