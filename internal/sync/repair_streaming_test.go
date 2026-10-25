package sync

import (
	"context"
	"errors"
	"testing"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

type streamingRepairTarget struct {
	*repairIndexTarget
	t            *testing.T
	reads, walks int
	failure      error
}

func (r *streamingRepairTarget) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	r.t.Fatal("repair materialized the physical listing")
	return nil, nil
}
func (r *streamingRepairTarget) WalkBackupMessages(_ context.Context, visit func(plugins.BackupMessage) error) error {
	r.walks++
	r.reads = 0
	for i := 0; i < 2; i++ {
		if r.reads != i {
			r.t.Fatalf("advanced listing before reading entry %d", i)
		}
		if err := visit(r.entry); err != nil {
			return err
		}
	}
	return r.failure
}
func (r *streamingRepairTarget) ReadBackupMessage(ctx context.Context, locator string) (*plugins.Message, error) {
	r.reads++
	return r.repairIndexTarget.ReadBackupMessage(ctx, locator)
}
func TestRepairConsumesEntriesAndRejectsPartialIndex(t *testing.T) {
	for _, failure := range []error{nil, plugins.ErrBackupListingLimit} {
		target := &streamingRepairTarget{repairIndexTarget: &repairIndexTarget{name: "fixture", entry: plugins.BackupMessage{Locator: "object"}, raw: []byte("raw fixture")}, t: t, failure: failure}
		engine := &SyncEngine{}
		locator, found, err := engine.findBackupByHash(context.Background(), target, &plugins.Message{}, dedup.ComputeContentHash(target.raw))
		if !errors.Is(err, failure) {
			t.Fatalf("error=%v", err)
		}
		if failure == nil {
			if !found || locator != "object" || target.reads != 2 {
				t.Fatalf("found=%v, locator=%q, reads=%d", found, locator, target.reads)
			}
		} else if _, loaded := engine.repairCache[target.Name()]; loaded || found {
			t.Fatal("budget failure published a partial repair index")
		}
	}
}
