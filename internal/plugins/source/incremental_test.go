package source

import (
	"context"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestIMAPSkipsKnownUIDsBeforeDownloading(t *testing.T) {
	src, _ := datedIMAPSource(t, []string{"01-Jan-2024 00:00:00 +0000"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	called := false
	ctx = plugins.WithKnownUIDs(ctx, func(scope, folder string) (map[string]bool, error) {
		called = true
		if scope == "" || folder != "INBOX" {
			t.Errorf("invalid scope/folder: %q %q", scope, folder)
		}
		return map[string]bool{"1": true}, nil
	})
	messages, errs := src.FetchMessages(ctx, []string{"INBOX"}, 0, 0)
	for msg := range messages {
		t.Errorf("downloaded known UID: %s", msg.UID)
	}
	for err := range errs {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("incremental lookup not called")
	}
}
