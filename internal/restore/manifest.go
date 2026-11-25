package restore

import (
	"fmt"
	"sort"

	"github.com/zekihan/mailvault/internal/plugins"
)

func MergeBackupMessages(physical, manifest []plugins.BackupMessage) ([]plugins.BackupMessage, error) {
	return MergeBackupMessagesCanonical(physical, manifest, func(locator string) string { return locator })
}

func MergeBackupMessagesCanonical(physical, manifest []plugins.BackupMessage, canonical func(string) string) ([]plugins.BackupMessage, error) {
	if canonical == nil {
		canonical = func(locator string) string { return locator }
	}
	physicalByLocator, err := indexPhysicalMessages(physical, canonical)
	if err != nil {
		return nil, err
	}
	manifestByLocator, err := indexManifestMessages(manifest, physicalByLocator, canonical)
	if err != nil {
		return nil, err
	}
	return mergeIndexedMessages(physicalByLocator, manifestByLocator), nil
}

func indexPhysicalMessages(physical []plugins.BackupMessage, canonical func(string) string) (map[string]plugins.BackupMessage, error) {
	byLocator := make(map[string]plugins.BackupMessage, len(physical))
	for i := range physical {
		entry := &physical[i]
		if entry.Locator == "" {
			return nil, fmt.Errorf("backup target returned an empty physical locator: %w", plugins.ErrBackupMessageCorrupt)
		}
		if _, exists := byLocator[canonical(entry.Locator)]; !exists {
			byLocator[canonical(entry.Locator)] = *entry
		}
	}
	return byLocator, nil
}

func indexManifestMessages(manifest []plugins.BackupMessage, physical map[string]plugins.BackupMessage, canonical func(string) string) (map[string]plugins.BackupMessage, error) {
	byLocator := make(map[string]plugins.BackupMessage, len(manifest))
	for i := range manifest {
		entry := &manifest[i]
		if entry.Locator == "" {
			return nil, fmt.Errorf("backup manifest contains an empty locator: %w", plugins.ErrBackupMessageCorrupt)
		}
		key := canonical(entry.Locator)
		if _, exists := physical[key]; !exists {
			return nil, fmt.Errorf("manifest locator %q has no physical backup message: %w", entry.Locator, plugins.ErrBackupMessageMissing)
		}
		if err := validateManifestAlias(byLocator, key, entry); err != nil {
			return nil, err
		}
		current, exists := byLocator[key]
		if !exists || chooseManifestEntry(&current, entry) {
			byLocator[key] = *entry
		}
	}
	return byLocator, nil
}

func validateManifestAlias(byLocator map[string]plugins.BackupMessage, key string, entry *plugins.BackupMessage) error {
	current, exists := byLocator[key]
	if exists && current.RawSHA256 != "" && entry.RawSHA256 != "" && current.RawSHA256 != entry.RawSHA256 {
		return fmt.Errorf("manifest aliases disagree on raw hash for locator %q: %w", entry.Locator, plugins.ErrBackupMessageCorrupt)
	}
	return nil
}

func chooseManifestEntry(current, entry *plugins.BackupMessage) bool {
	if current.RawSHA256 == "" && entry.RawSHA256 != "" {
		return true
	}
	return current.RawSHA256 == entry.RawSHA256 && aliasLess(entry, current)
}

func mergeIndexedMessages(physical, manifest map[string]plugins.BackupMessage) []plugins.BackupMessage {
	locators := make([]string, 0, len(physical))
	for key := range physical {
		locators = append(locators, key)
	}
	sort.Strings(locators)
	merged := make([]plugins.BackupMessage, 0, len(locators))
	for _, key := range locators {
		if entry, ok := manifest[key]; ok {
			merged = append(merged, entry)
			continue
		}
		entry := physical[key]
		entry.RawSHA256 = ""
		merged = append(merged, entry)
	}
	return merged
}

func aliasLess(first, second *plugins.BackupMessage) bool {
	if first.Source != second.Source {
		return first.Source < second.Source
	}
	if first.Scope != second.Scope {
		return first.Scope < second.Scope
	}
	if first.Folder != second.Folder {
		return first.Folder < second.Folder
	}
	return first.UID < second.UID
}
