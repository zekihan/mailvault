package sync

import (
	"context"
	"fmt"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

func (e *SyncEngine) validateRepairedBackups(ctx context.Context, src plugins.Source, tgt plugins.Target) []error {
	reader, ok := tgt.(plugins.TargetBackupReader)
	if !ok {
		return []error{fmt.Errorf("target %q does not support verification", tgt.Name())}
	}
	entries, err := e.stateStore.BackupMessages(ctx, tgt.Name())
	if err != nil {
		return []error{err}
	}
	var failures []error
	checked := make(map[string]bool)
	for _, entry := range entries {
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
		msg, err := reader.ReadBackupMessage(ctx, entry.Locator)
		if err != nil {
			failures = append(failures, fmt.Errorf("repair unresolved for %q (source folder %q, UID %s): %w", entry.Locator, entry.Folder, entry.UID, err))
			continue
		}
		if dedup.ComputeContentHash(msg.Raw) != entry.RawSHA256 {
			failures = append(failures, fmt.Errorf("repair unresolved for %q (source folder %q, UID %s): checksum mismatch; source may no longer contain the message", entry.Locator, entry.Folder, entry.UID))
		}
	}
	legacy, err := e.stateStore.LegacyBackupCandidates(ctx, tgt.Name())
	if err != nil {
		return append(failures, err)
	}
	for _, entry := range legacy {
		if entry.Source != src.Name() {
			continue
		}
		if entry.RawSHA256 == "" {
			failures = append(failures, fmt.Errorf("repair unresolved for legacy source folder %q UID %s: no historical checksum or current source copy", entry.Folder, entry.UID))
			continue
		}
		_, found, err := e.findBackupByHash(ctx, tgt, &plugins.Message{Folder: entry.Folder}, entry.RawSHA256)
		if err != nil {
			failures = append(failures, err)
		} else if !found {
			failures = append(failures, fmt.Errorf("repair unresolved for legacy source folder %q UID %s: no matching backup or current source copy", entry.Folder, entry.UID))
		}
	}
	return failures
}
