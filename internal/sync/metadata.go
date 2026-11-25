package sync

import (
	"context"
	"fmt"

	"github.com/zekihan/mailvault/internal/plugins"
)

// WithRefreshFlags updates metadata for already delivered IMAP messages.
func WithRefreshFlags(enabled bool) SyncOption {
	return func(engine *SyncEngine) { engine.refreshFlags = enabled }
}

func (engine *SyncEngine) refreshMetadata(ctx context.Context, src plugins.Source, tgt plugins.Target, msg *plugins.Message) error {
	entry, found, err := engine.stateStore.FindBackupMessage(ctx, src.Name(), plugins.TargetStateKey(tgt), targetReceiptScope(tgt, msg.UIDScope), msg.Folder, msg.UID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no integrity manifest for UID %s in %q; run repair before refreshing flags", msg.UID, msg.Folder)
	}
	if engine.dryRun {
		return nil
	}
	value, _ := engine.deliveryLocks.LoadOrStore(tgt.Name(), &targetDelivery{seen: make(map[string]bool)})
	delivery, ok := value.(*targetDelivery)
	if !ok {
		return fmt.Errorf("delivery lock for %q has unexpected type %T", tgt.Name(), value)
	}
	delivery.mu.Lock()
	defer delivery.mu.Unlock()
	if updater, ok := tgt.(interface {
		UpdateBackupMetadata(ctx context.Context, entry *plugins.BackupMessage, flags []string) (string, error)
	}); ok {
		locator, err := updater.UpdateBackupMetadata(ctx, &entry, msg.Flags)
		if err != nil {
			return err
		}
		entry.Locator = locator
	}
	entry.Flags = append([]string(nil), msg.Flags...)
	return engine.stateStore.RecordBackupMessage(ctx, &entry)
}
