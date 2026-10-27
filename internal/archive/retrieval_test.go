package archive

import (
	"context"
	"github.com/algorhythmic/skald/sessioncapture"
	"reflect"
	"strings"
	"testing"
)

func TestRetrievalSnapshotScopeLimitsAndRevocation(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, testBatch(t, raw, r, sessioncapture.Checkpoint{}))
	if err := s.SetReadScope(ctx, []string{r.Namespace}); err != nil {
		t.Fatal(err)
	}
	generation, err := s.ReadScopeGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.RetrievalSnapshot(ctx, []string{r.Namespace}, generation, 1000, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) == 0 {
		t.Fatal("empty message corpus")
	}
	for _, r := range snapshot.Records {
		if r.Role != "user" && r.Role != "assistant" {
			t.Fatal("forbidden role")
		}
	}
	if err := s.ValidateRetrieval(ctx, []string{r.Namespace}, snapshot, snapshot.Records); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetrievalSnapshot(ctx, []string{r.Namespace}, generation, 1000, 1); err == nil {
		t.Fatal("must fail full corpus over byte cap")
	}
	if _, err := s.RetrievalSnapshot(ctx, []string{"unpermitted"}, generation, 1000, 8<<20); err == nil {
		t.Fatal("accepted foreign scope")
	}
	if err := s.SetReadScope(ctx, []string{}); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateRetrieval(ctx, []string{r.Namespace}, snapshot, snapshot.Records); err == nil {
		t.Fatal("revocation must invalidate old snapshot")
	}
}
func TestRetrievalSnapshotRedactionInvalidatesExactCitations(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, testBatch(t, raw, r, sessioncapture.Checkpoint{}))
	if err := s.SetReadScope(ctx, []string{r.Namespace}); err != nil {
		t.Fatal(err)
	}
	g, _ := s.ReadScopeGeneration(ctx)
	snap, err := s.RetrievalSnapshot(ctx, []string{r.Namespace}, g, 1000, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Redact(ctx, r.Namespace, "record", snap.Records[0].Ref.RecordKey); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateRetrieval(ctx, []string{r.Namespace}, snap, snap.Records[:1]); err == nil {
		t.Fatal("redacted citation accepted")
	}
}

func TestRetrievalCorpusRevisionAndCurrentHeads(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	first := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, first)
	other := r
	other.Namespace = "fixture:other"
	other.StreamID = "other"
	if err := s.Register(ctx, []Registration{r, other}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReadScope(ctx, []string{r.Namespace, "fixture:other"}); err != nil {
		t.Fatal(err)
	}
	generation, _ := s.ReadScopeGeneration(ctx)
	ns := []string{r.Namespace}
	snapshot, err := s.RetrievalSnapshot(ctx, ns, generation, 1000, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Metadata for a different namespace advances the global boundary only.
	if _, err := s.db.ExecContext(ctx, "INSERT INTO changes(change_kind,namespace,payload_json) VALUES('source','fixture:other','{}')"); err != nil {
		t.Fatal(err)
	}
	identity, err := s.RetrievalIdentity(ctx, ns, generation)
	if err != nil {
		t.Fatal(err)
	}
	if identity.CorpusRevision != snapshot.CorpusRevision || identity.Boundary == snapshot.Boundary {
		t.Fatal("unrelated change invalidated scoped corpus revision")
	}
	if err := s.ValidateRetrieval(ctx, ns, snapshot, snapshot.Records); err != nil {
		t.Fatal("unrelated change invalidated citations", err)
	}
	// Appending a new record may race a cold publication without making its
	// previously captured current-head evidence stale.
	appended := append(append([]byte{}, raw...), []byte("{\"type\":\"assistant\",\"uuid\":\"new-append\",\"sessionId\":\"synthetic-claude\",\"message\":{\"role\":\"assistant\",\"content\":\"A new synthetic append.\"}}\n")...)
	second := testBatch(t, appended, r, first.Checkpoint)
	mustIngest(t, s, r, first.Checkpoint, second)
	if err := s.ValidateRetrieval(ctx, ns, snapshot, snapshot.Records); err != nil {
		t.Fatal("append invalidated current citations", err)
	}
	next, err := s.RetrievalIdentity(ctx, ns, generation)
	if err != nil || next.CorpusRevision <= identity.CorpusRevision {
		t.Fatal("append did not advance corpus revision", err)
	}
	var selected RetrievalRecord
	for _, record := range snapshot.Records {
		if strings.Contains(record.Text, "I will check the archive boundaries.") {
			selected = record
		}
	}
	if selected.ID == "" {
		t.Fatal("missing correction fixture")
	}
	changed := strings.Replace(string(appended), "I will check the archive boundaries.", "Revised retrieval answer.", 1)
	correction := testBatch(t, []byte(changed), r, second.Checkpoint)
	mustIngest(t, s, r, second.Checkpoint, correction)
	if err := s.ValidateRetrieval(ctx, ns, snapshot, []RetrievalRecord{selected}); err == nil {
		t.Fatal("corrected old head accepted")
	}
	fresh, err := s.RetrievalSnapshot(ctx, ns, generation, 1000, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	revised := false
	for _, record := range fresh.Records {
		if strings.Contains(record.Text, "I will check the archive boundaries.") {
			t.Fatal("superseded text leaked into current corpus")
		}
		if record.Text == "Revised retrieval answer." {
			revised = true
		}
	}
	if !revised {
		t.Fatal("corrected current text missing")
	}
}

func TestRetrievalBudgetCountsProjectionAndCompressedSnapshotMatches(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	inflated := strings.Replace(string(raw), "README.md", strings.Repeat("synthetic-input", 1<<16), 1)
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, testBatch(t, []byte(inflated), r, sessioncapture.Checkpoint{}))
	if err := s.SetReadScope(ctx, []string{r.Namespace}); err != nil {
		t.Fatal(err)
	}
	g, _ := s.ReadScopeGeneration(ctx)
	before, err := s.RetrievalSnapshot(ctx, []string{r.Namespace}, g, 1000, 8192)
	if err != nil {
		t.Fatal("large discarded tool metadata exhausted projected text budget", err)
	}
	if len(before.Records) == 0 {
		t.Fatal("empty projection")
	}
	if _, err := s.CompressPayloads(ctx, 1000, 8<<20); err != nil {
		t.Fatal(err)
	}
	after, err := s.RetrievalSnapshot(ctx, []string{r.Namespace}, g, 1000, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("compressed current-head projection changed")
	}
}
