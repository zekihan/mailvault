package sync

import (
	"context"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

type placementTarget struct{ *fixtureTarget }

func (t *placementTarget) ReceiptScope() string { return "folders-v1" }
func (t *placementTarget) DeliveryIdentity(msg *plugins.Message, identity string) string {
	return identity + "\x00" + msg.Folder
}

func TestPreservedPlacementsDoNotReuseFlatReceipts(t *testing.T) {
	f := newEngineFixture(t)
	f.cfg.Pairs[0].Targets = []string{"first"}
	f.source.messages = []*plugins.Message{
		{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: []byte("Subject: same\r\n\r\nbody")},
		{UID: "2", UIDScope: "generation", Folder: "Starred", Raw: []byte("Subject: same\r\n\r\nbody")},
	}
	run := func(want int) {
		t.Helper()
		engine, err := NewSyncEngine(f.cfg, f.store, f.registry)
		if err != nil {
			t.Fatal(err)
		}
		stats, err := engine.Run(context.Background())
		if err != nil || stats.TotalErrors != 0 || stats.TotalWritten != want {
			t.Fatalf("run = %+v %v, want %d written", stats, err, want)
		}
	}
	run(1)
	wrapped := &placementTarget{f.targets["first"]}
	f.registry.RegisterTarget("fixture", func(map[string]interface{}) (plugins.Target, error) { return wrapped, nil })
	known, err := f.store.KnownUIDs(context.Background(), "account", "first", targetReceiptScope(wrapped, "generation"), "INBOX")
	if err != nil || len(known) != 0 {
		t.Fatalf("flat receipts leaked into preserved mode: %v %v", known, err)
	}
	run(2)
	run(0)
}
