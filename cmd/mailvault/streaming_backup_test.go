package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

type streamingVerificationReader struct {
	t       *testing.T
	reads   int
	failure error
}

func (r *streamingVerificationReader) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	r.t.Fatal("consumer materialized the physical listing")
	return nil, nil
}
func (r *streamingVerificationReader) WalkBackupMessages(_ context.Context, visit func(plugins.BackupMessage) error) error {
	for i := 0; i < 2; i++ {
		if r.reads != i {
			r.t.Fatalf("advanced listing before reading entry %d", i)
		}
		if err := visit(plugins.BackupMessage{Locator: fmt.Sprint(i)}); err != nil {
			return err
		}
	}
	return r.failure
}
func (r *streamingVerificationReader) ReadBackupMessage(context.Context, string) (*plugins.Message, error) {
	r.reads++
	return &plugins.Message{Raw: []byte("Subject: fixture\n\nbody\n")}, nil
}
func TestVerificationConsumesEntriesBeforeAdvancingListing(t *testing.T) {
	previous := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = previous })
	for _, failure := range []error{nil, plugins.ErrBackupListingLimit} {
		reader := &streamingVerificationReader{t: t, failure: failure}
		report, err := verifyTarget(context.Background(), "fixture", reader)
		if !errors.Is(err, failure) || report.Scanned != 2 {
			t.Fatalf("report=%+v, error=%v", report, err)
		}
		if failure == nil && report.Unverifiable != 2 {
			t.Fatalf("report=%+v", report)
		}
	}
}
