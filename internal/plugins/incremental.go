package plugins

import "context"

type knownUIDsKey struct{}

// WithKnownUIDs supplies successful deliveries for a mailbox generation.
func WithKnownUIDs(ctx context.Context, lookup func(string, string) (map[string]bool, error)) context.Context {
	return context.WithValue(ctx, knownUIDsKey{}, lookup)
}

func KnownUIDs(ctx context.Context, scope, folder string) (map[string]bool, error) {
	if lookup, ok := ctx.Value(knownUIDsKey{}).(func(string, string) (map[string]bool, error)); ok {
		return lookup(scope, folder)
	}
	return nil, nil
}
