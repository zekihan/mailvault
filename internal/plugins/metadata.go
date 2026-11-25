package plugins

import "context"

type metadataObserverKey struct{}

// WithMetadataObserver enables metadata-only refresh for previously delivered UIDs.
func WithMetadataObserver(ctx context.Context, observe func(*Message) error) context.Context {
	return context.WithValue(ctx, metadataObserverKey{}, observe)
}

func MetadataObserver(ctx context.Context) func(*Message) error {
	if observe, ok := ctx.Value(metadataObserverKey{}).(func(*Message) error); ok {
		return observe
	}
	return nil
}
