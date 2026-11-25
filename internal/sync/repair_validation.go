package sync

import (
	"context"
	"fmt"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

func checkRepairEntry(ctx context.Context, reader plugins.TargetBackupReader, entry *plugins.BackupMessage) error {
	msg, err := reader.ReadBackupMessage(ctx, entry.Locator)
	if err != nil {
		return fmt.Errorf("repair unresolved for %q (source folder %q, UID %s): %w", entry.Locator, entry.Folder, entry.UID, err)
	}
	if dedup.ComputeContentHash(msg.Raw) != entry.RawSHA256 {
		return fmt.Errorf("repair unresolved for %q (source folder %q, UID %s): checksum mismatch; source may no longer contain the message", entry.Locator, entry.Folder, entry.UID)
	}
	return nil
}

func checkRepairLegacyEntry(ctx context.Context, engine *SyncEngine, tgt plugins.Target, entry *plugins.BackupMessage) error {
	_, found, err := engine.findBackupByHash(ctx, tgt, &plugins.Message{Folder: entry.Folder}, entry.RawSHA256)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("repair unresolved for legacy source folder %q UID %s: no matching backup or current source copy", entry.Folder, entry.UID)
	}
	return nil
}

func validateLegacyEntries(ctx context.Context, engine *SyncEngine, src plugins.Source, tgt plugins.Target, legacy []plugins.BackupMessage, failures []error) []error {
	for i := range legacy {
		entry := &legacy[i]
		if entry.Source != src.Name() {
			continue
		}
		if entry.RawSHA256 == "" {
			failures = append(failures, fmt.Errorf("repair unresolved for legacy source folder %q UID %s: no historical checksum or current source copy", entry.Folder, entry.UID))
			continue
		}
		if err := checkRepairLegacyEntry(ctx, engine, tgt, entry); err != nil {
			failures = append(failures, err)
		}
	}
	return failures
}

func (engine *SyncEngine) validateRepairedBackups(ctx context.Context, src plugins.Source, tgt plugins.Target) []error {
	reader, ok := tgt.(plugins.TargetBackupReader)
	if !ok {
		return []error{fmt.Errorf("target %q does not support verification", tgt.Name())}
	}
	stateTarget := plugins.TargetStateKey(tgt)
	entries, err := engine.stateStore.BackupMessages(ctx, stateTarget)
	if err != nil {
		return []error{err}
	}
	var failures []error
	checked := make(map[string]bool)
	for i := range entries {
		entry := &entries[i]
		if entry.Source != src.Name() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return append(failures, err)
		}
		key := entry.Locator + "\x00" + entry.RawSHA256
		if checked[key] {
			continue
		}
		checked[key] = true
		if err := checkRepairEntry(ctx, reader, entry); err != nil {
			failures = append(failures, err)
		}
	}
	legacy, err := engine.stateStore.LegacyBackupCandidates(ctx, stateTarget)
	if err != nil {
		return append(failures, err)
	}
	return validateLegacyEntries(ctx, engine, src, tgt, legacy, failures)
}
