package sync

import (
	"bytes"
	"testing"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

func TestEnginePreservesDistinctContentSharingMessageID(t *testing.T) {
	fixture := newEngineFixture(t)
	fixture.cfg.Pairs[0].Targets = []string{"first"}
	original := fixture.source.messages[0]
	variant := *original
	variant.UID = "43"
	variant.Raw = bytes.Replace(original.Raw, []byte("body"), []byte("different body"), 1)
	variant.ContentHash = dedup.ComputeContentHash(variant.Raw)
	fixture.source.messages = []*plugins.Message{original, &variant}
	assertStats(t, fixture.run(t), 2, 0, 0)
	if fixture.targets["first"].writes != 2 {
		t.Fatal("distinct content was not delivered")
	}
	records, err := fixture.store.FindByContentHash(t.Context(), variant.ContentHash)
	if err != nil || len(records) != 1 {
		t.Fatalf("variant not persisted: records=%d err=%v", len(records), err)
	}
	assertStats(t, fixture.run(t), 0, 2, 0)
}

func TestEngineDoesNotTrustLegacyMessageIDReceipts(t *testing.T) {
	fixture := newEngineFixture(t)
	fixture.cfg.Pairs[0].Targets = []string{"first"}
	original := fixture.source.messages[0]
	if err := fixture.store.MarkDelivered(t.Context(), "first", "mid:"+original.MessageID, "account", original.Folder, original.UID); err != nil {
		t.Fatal(err)
	}
	fixture.source.messages = []*plugins.Message{original}
	assertStats(t, fixture.run(t), 1, 0, 0)
	assertStats(t, fixture.run(t), 0, 1, 0)
}

func TestEngineDeduplicatesIdenticalRawContentAcrossMetadata(t *testing.T) {
	fixture := newEngineFixture(t)
	fixture.cfg.Pairs[0].Targets = []string{"first"}
	original := fixture.source.messages[0]
	duplicate := *original
	duplicate.UID = "43"
	duplicate.Folder = "Other"
	duplicate.MessageID = "different-envelope@example.invalid"
	fixture.source.messages = []*plugins.Message{original, &duplicate}
	assertStats(t, fixture.run(t), 1, 1, 0)
}
