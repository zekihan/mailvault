package restore

import (
	"fmt"
	"sort"

	"github.com/zekihan/mailvault/internal/plugins"
)

func MergeBackupMessages(physical, manifest []plugins.BackupMessage) ([]plugins.BackupMessage, error) {
	physicalByLocator := make(map[string]plugins.BackupMessage, len(physical))
	for _, entry := range physical {
		if entry.Locator == "" {
			return nil, fmt.Errorf("backup target returned an empty physical locator: %w", plugins.ErrBackupMessageCorrupt)
		}
		if _, exists := physicalByLocator[entry.Locator]; !exists {
			physicalByLocator[entry.Locator] = entry
		}
	}

	manifestByLocator := make(map[string]plugins.BackupMessage, len(manifest))
	for _, entry := range manifest {
		if entry.Locator == "" {
			return nil, fmt.Errorf("backup manifest contains an empty locator: %w", plugins.ErrBackupMessageCorrupt)
		}
		if _, exists := physicalByLocator[entry.Locator]; !exists {
			return nil, fmt.Errorf("manifest locator %q has no physical backup message: %w", entry.Locator, plugins.ErrBackupMessageMissing)
		}
		current, exists := manifestByLocator[entry.Locator]
		if exists && current.RawSHA256 != "" && entry.RawSHA256 != "" && current.RawSHA256 != entry.RawSHA256 {
			return nil, fmt.Errorf("manifest aliases disagree on raw hash for locator %q: %w", entry.Locator, plugins.ErrBackupMessageCorrupt)
		}
		choose := !exists
		if exists {
			if current.RawSHA256 == "" && entry.RawSHA256 != "" {
				choose = true
			} else if current.RawSHA256 == entry.RawSHA256 && aliasLess(entry, current) {
				choose = true
			}
		}
		if choose {
			manifestByLocator[entry.Locator] = entry
		}
	}

	locators := make([]string, 0, len(physicalByLocator))
	for locator := range physicalByLocator {
		locators = append(locators, locator)
	}
	sort.Strings(locators)
	merged := make([]plugins.BackupMessage, 0, len(locators))
	for _, locator := range locators {
		if entry, ok := manifestByLocator[locator]; ok {
			merged = append(merged, entry)
			continue
		}
		entry := physicalByLocator[locator]
		entry.RawSHA256 = ""
		merged = append(merged, entry)
	}
	return merged, nil
}

func aliasLess(a, b plugins.BackupMessage) bool {
	if a.Source != b.Source {
		return a.Source < b.Source
	}
	if a.Scope != b.Scope {
		return a.Scope < b.Scope
	}
	if a.Folder != b.Folder {
		return a.Folder < b.Folder
	}
	return a.UID < b.UID
}
