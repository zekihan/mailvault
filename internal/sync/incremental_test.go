package sync

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestIncrementalResumesAfterDeliveryReceiptFailure(t *testing.T) {
	ctx := t.Context()
	root := physicalTempDir(t)
	statePath := filepath.Join(physicalTempDir(t), "state.db")
	msg := &plugins.Message{
		UID: "42", UIDScope: "generation-1", Folder: "Archive",
		Raw: []byte("Subject: delivery checkpoint\r\n\r\nsynthetic body\r\n"),
	}
	source := &fixtureSource{name: "account", messages: []*plugins.Message{msg}}
	var registeredSource plugins.Source = &incrementalFixtureSource{source}
	cfg, registry := repairReceiptFixture(root, registeredSource)
	store := openReceiptTestStore(ctx, t, statePath)
	addDeliveryFailureTrigger(ctx, t, statePath)

	stats := runReceiptTestEngine(ctx, t, cfg, store, registry)
	assertStats(t, stats, 1, 0, 1)
	if err := stats.Results[0].Errors[0]; !strings.Contains(err.Error(), "forced delivery receipt failure") {
		t.Fatalf("delivery error = %v", err)
	}
	assertMaildirObjectCount(ctx, t, root, 1)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}
	_, dropErr := db.ExecContext(ctx, "DROP TRIGGER fail_delivery_receipt")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if dropErr != nil {
		t.Fatal(dropErr)
	}

	store = openReceiptTestStore(ctx, t, statePath)
	stats = runReceiptTestEngine(ctx, t, cfg, store, registry)
	assertStats(t, stats, 0, 1, 0)
	assertMaildirObjectCount(ctx, t, root, 1)
	assertRepairReceipts(ctx, t, store, root, msg)

	stats = runReceiptTestEngine(ctx, t, cfg, store, registry)
	if stats.TotalFetched != 0 || stats.TotalErrors != 0 {
		t.Fatalf("resumed delivery did not persist its UID receipt: %+v", stats)
	}
}

func assertDeliveryReceipt(ctx context.Context, t *testing.T, fixture *engineFixture, mode, target string) {
	t.Helper()
	known, err := fixture.store.KnownUIDs(ctx, "account", target, "generation-1", "Archive")
	if err != nil {
		t.Fatal(err)
	}
	want := mode != "dry-run" && (mode != "failure" || target != "first")
	if known["42"] != want {
		t.Fatalf("%s receipt = %v, want %v", target, known, want)
	}
}

func assertReceiptScopeIsolation(ctx context.Context, t *testing.T, fixture *engineFixture) {
	t.Helper()
	for _, query := range [][4]string{{"account", "new-target", "generation-1", "Archive"}, {"account", "first", "generation-2", "Archive"}, {"account", "first", "generation-1", "Other"}} {
		known, err := fixture.store.KnownUIDs(ctx, query[0], query[1], query[2], query[3])
		if err != nil || len(known) != 0 {
			t.Fatalf("receipt leaked across scope: %v %v", known, err)
		}
	}
}

func runReceiptDeliveryMode(t *testing.T, mode string) {
	t.Helper()
	fixture := newEngineFixture(t)
	for _, msg := range fixture.source.messages {
		msg.UIDScope = "generation-1"
	}
	fixture.targets["first"].fail = mode == "failure"
	engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry, WithDryRun(mode == "dry-run"), WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"first", "second"} {
		assertDeliveryReceipt(ctx, t, fixture, mode, target)
	}
	assertReceiptScopeIsolation(ctx, t, fixture)
}

func TestIMAPReceiptsRequireSuccessfulDelivery(t *testing.T) {
	for _, mode := range []string{"success", "failure", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			runReceiptDeliveryMode(t, mode)
		})
	}
}

// incrementalFixtureSource exercises the source/engine receipt contract without a network account.
type incrementalFixtureSource struct {
	*fixtureSource
}

func (source *incrementalFixtureSource) FetchMessages(ctx context.Context, folders []string, since, until int64) (<-chan *plugins.Message, <-chan error) {
	messages := make(chan *plugins.Message, len(source.messages))
	failures := make(chan error, 1)
	defer close(messages)
	defer close(failures)
	known, err := plugins.KnownUIDs(ctx, "generation-1", "Archive")
	if err != nil {
		failures <- err
		return messages, failures
	}
	for _, msg := range source.messages {
		if !known[msg.UID] {
			messages <- msg
		}
	}
	return messages, failures
}

func TestIncrementalSecondRunAndForceFetchAll(t *testing.T) {
	fixture := newEngineFixture(t)
	fixture.cfg.Pairs = fixture.cfg.Pairs[:1]
	fixture.cfg.Pairs[0].Targets = []string{"first"}
	fixture.source.messages = fixture.source.messages[:1]
	fixture.source.messages[0].UIDScope = "generation-1"
	fixture.registry.RegisterSource("fixture", func(map[string]any) (plugins.Source, error) {
		return &incrementalFixtureSource{fixture.source}, nil
	})
	run := func(force bool, wantFetched int) {
		t.Helper()
		engine, err := NewSyncEngine(fixture.cfg, fixture.store, fixture.registry, WithForceFetchAll(force))
		if err != nil {
			t.Fatal(err)
		}
		stats, err := engine.Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if stats.TotalErrors != 0 || stats.TotalFetched != wantFetched {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	}
	run(false, 1)
	run(false, 0)
	fixture.source.messages = append(fixture.source.messages, &plugins.Message{UID: "43", UIDScope: "generation-1", Folder: "Archive", Raw: []byte("Subject: new\r\n\r\nnew body")}) //nolint:dupword // fixture message content, not prose
	run(false, 1)
	run(true, 2)
	if fixture.targets["first"].writes != 2 {
		t.Fatalf("force fetch duplicated backups: %d writes", fixture.targets["first"].writes)
	}
}
