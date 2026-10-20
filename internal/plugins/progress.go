package plugins

import "context"

// FetchProgress counts messages downloaded in the current folder before local
// date filtering and deduplication. Total is -1 until the server search completes.
type FetchProgress struct {
	Folder  string
	Fetched int
	Total   int
	Stage   string
}

type fetchProgressKey struct{}

// WithFetchProgress attaches an observer to one fetch invocation, avoiding shared
// callbacks when multiple pairs use the same source.
func WithFetchProgress(ctx context.Context, report func(FetchProgress)) context.Context {
	return context.WithValue(ctx, fetchProgressKey{}, report)
}

// ReportFetchProgress publishes a source's current folder and download counts.
func ReportFetchProgress(ctx context.Context, progress FetchProgress) {
	if report, ok := ctx.Value(fetchProgressKey{}).(func(FetchProgress)); ok && report != nil {
		report(progress)
	}
}
