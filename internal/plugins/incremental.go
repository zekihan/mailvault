package plugins

import "context"

type knownUIDsKey struct{}
type folderStateObserverKey struct{}

// FolderStateObservation reports server state observed after selecting a folder.
// These values are informational and must not replace per-target delivery receipts.
type FolderStateObservation struct {
	Folder      string
	UIDValidity string
	UIDNext     string
	ModSeq      string
}

// WithFolderStateObserver attaches a callback for selected-folder observations.
func WithFolderStateObserver(ctx context.Context, observe func(FolderStateObservation) error) context.Context {
	return context.WithValue(ctx, folderStateObserverKey{}, observe)
}

// ObserveFolderState reports folder state when a caller has installed an observer.
func ObserveFolderState(ctx context.Context, observation FolderStateObservation) error {
	if observe, ok := ctx.Value(folderStateObserverKey{}).(func(FolderStateObservation) error); ok {
		return observe(observation)
	}
	return nil
}

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
