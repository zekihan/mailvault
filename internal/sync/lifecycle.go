package sync

import (
	"errors"
	"fmt"
)

// closeResources is used on successful runs and every startup/processing return
// path. Targets may hold resources even when Initialize only partially succeeds.
func (engine *SyncEngine) closeResources(stats *SyncStats) error {
	var failures []error
	record := func(err error) {
		if err == nil {
			return
		}
		failures = append(failures, err)
		stats.FinalizationErrors = append(stats.FinalizationErrors, err.Error())
		stats.TotalErrors++
	}
	if !engine.dryRun {
		for _, target := range engine.targets {
			if err := target.Close(); err != nil {
				record(fmt.Errorf("closing target %q: %w", target.Name(), err))
			}
		}
	}
	for _, source := range engine.sources {
		if err := source.Close(); err != nil {
			record(fmt.Errorf("closing source %q: %w", source.Name(), err))
		}
	}
	return errors.Join(failures...)
}
