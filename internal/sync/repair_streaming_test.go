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

func (target *streamingRepairTarget) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	target.t.Fatal("repair materialized the physical listing")
	return nil, nil
}
func (target *streamingRepairTarget) WalkBackupMessages(_ context.Context, visit func(plugins.BackupMessage) error) error {
	target.walks++
	target.reads = 0
	for i := range 2 {
		if target.reads != i {
			target.t.Fatalf("advanced listing before reading entry %d", i)
		}
		if err := visit(target.entry); err != nil {
			return err
		}
	}
	return target.failure
}
func (target *streamingRepairTarget) ReadBackupMessage(ctx context.Context, locator string) (*plugins.Message, error) {
	target.reads++
	return target.repairIndexTarget.ReadBackupMessage(ctx, locator)
}
func TestRepairConsumesEntriesAndRejectsPartialIndex(t *testing.T) {
	for _, failure := range []error{nil, plugins.ErrBackupListingLimit} {
		target := &streamingRepairTarget{repairIndexTarget: &repairIndexTarget{name: "fixture", entry: plugins.BackupMessage{Locator: "object"}, raw: []byte("raw fixture")}, t: t, failure: failure}
		engine := &SyncEngine{}
		locator, found, err := engine.findBackupByHash(t.Context(), target, &plugins.Message{}, dedup.ComputeContentHash(target.raw))
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
