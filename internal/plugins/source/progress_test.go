package source

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestIMAPFolderProgress(t *testing.T) {
	for _, count := range []int{0, 3} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			dates := make([]string, count)
			for i := range dates {
				dates[i] = "01-Jan-2024 00:00:00 +0000"
			}
			src, _ := datedIMAPSource(t, dates)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var updates []plugins.FetchProgress
			ctx = plugins.WithFetchProgress(ctx, func(progress plugins.FetchProgress) {
				updates = append(updates, progress)
			})
			messages, failures := src.FetchMessages(ctx, []string{"Archive"}, 0, 0)
			for messages != nil || failures != nil {
				select {
				case _, ok := <-messages:
					if !ok {
						messages = nil
					}
				case err, ok := <-failures:
					if !ok {
						failures = nil
					} else if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if len(updates) != count+3 {
				t.Fatalf("unexpected updates: %+v", updates)
			}
			if updates[0].Folder != "Archive" || updates[0].Total != -1 {
				t.Fatalf("missing folder while searching: %+v", updates[0])
			}
			for i := 0; i <= count; i++ {
				p := updates[i+1]
				if p.Folder != "Archive" || p.Fetched != i || p.Total != count {
					t.Fatalf("incorrect download progress: %+v", p)
				}
			}
			last := updates[len(updates)-1]
			if last.Stage != "finished" || last.Fetched != count || last.Total != count {
				t.Fatalf("incorrect final progress: %+v", last)
			}
		})
	}
}
