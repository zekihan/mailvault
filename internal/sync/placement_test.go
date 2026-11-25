package sync

import (
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

type placementTarget struct{ *fixtureTarget }

func (t *placementTarget) ReceiptScope() string { return "folders-v1" }
func (t *placementTarget) DeliveryIdentity(msg *plugins.Message, identity string) string {
	return identity + "\x00" + msg.Folder
}

func TestPreservedPlacementsDoNotReuseFlatReceipts(t *testing.T) {
	fixture := newEngineFixture(t)
	fixture.cfg.Pairs[0].Targets = []string{"first"}
	fixture.source.messages = []*plugins.Message{
		{UID: "1", UIDScope: "generation", Folder: "INBOX", Raw: []byte("Subject: same\r\n\r\nbody")},
		{UID: "2", UIDScope: "generation", Folder: "Starred", Raw: []byte("Subject: same\r\n\r\nbody")},
	}
	run := func(want int) {
		t.Helper()
		engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry)
		if err != nil {
			t.Fatal(err)
		}
		stats, err := engine.Run(t.Context())
		if err != nil || stats.TotalErrors != 0 || stats.TotalWritten != want {
			t.Fatalf("run = %+v %v, want %d written", stats, err, want)
		}
	}
	run(1)
	wrapped := &placementTarget{fixture.targets["first"]}
	fixture.registry.RegisterTarget("fixture", func(map[string]any) (plugins.Target, error) { return wrapped, nil })
	known, err := fixture.store.KnownUIDs(t.Context(), "account", "first", targetReceiptScope(wrapped, "generation"), "INBOX")
	if err != nil || len(known) != 0 {
		t.Fatalf("flat receipts leaked into preserved mode: %v %v", known, err)
	}
	run(2)
	run(0)
}
