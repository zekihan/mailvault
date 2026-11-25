package sync

import "testing"

// mustAssert returns v as T, failing the test if the assertion does not hold.
func mustAssert[T any](t *testing.T, v any) T {
	t.Helper()
	out, ok := v.(T)
	if !ok {
		t.Fatalf("unexpected type %T", v)
	}
	return out
}
