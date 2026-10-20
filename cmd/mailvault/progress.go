package main

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

func startSyncProgress(out io.Writer, dryRun bool) (func(string, string), func()) {
	var mu sync.Mutex
	statuses := make(map[string]string)
	started := time.Now()
	mode := "Sync"
	if dryRun {
		mode = "Dry run (fetches mail; no backup or state writes)"
	}
	_, _ = fmt.Fprintln(out, mode+" started; progress every 5s.")
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				mu.Lock()
				keys := make([]string, 0, len(statuses))
				for key := range statuses {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				for _, key := range keys {
					_, _ = fmt.Fprintf(out, "[%s] %s: %s\n", time.Since(started).Round(time.Second), key, statuses[key])
				}
				mu.Unlock()
			}
		}
	}()
	return func(key, status string) {
			mu.Lock()
			defer mu.Unlock()
			if _, exists := statuses[key]; !exists {
				_, _ = fmt.Fprintf(out, "%s: %s\n", key, status)
			}
			statuses[key] = status
		}, func() {
			close(done)
			<-stopped
		}
}
