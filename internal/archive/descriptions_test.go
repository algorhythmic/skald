package archive

import (
	"context"
	"fmt"
	"github.com/algorhythmic/skald/sessioncapture"
	"strings"
	"testing"
	"time"
)

func TestDescriptionsWholeArchiveAndScope(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	// More than a transcript page of trailing user activity cannot hide the recap
	// or the latest usable assistant message. User text is never the description.
	for i := 0; i < 40; i++ {
		raw = append(raw, []byte(fmt.Sprintf(`{"type":"user","uuid":"tail-%d","sessionId":"synthetic-claude","message":{"role":"user","content":"trailing user content"}}`+"\n", i))...)
	}
	b := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, b)
	key := *b.Records[0].Record.ConversationKey
	ns := []string{r.Namespace}
	d, e := s.Descriptions(ctx, ns, key)
	if e != nil {
		t.Fatal(e)
	}
	if d.Kind != "assistant_excerpt" || d.Selected == nil || !strings.Contains(d.Selected.Text, "source-removal fixture") || len(d.PriorRecaps) != 1 || !d.NewActivity || d.OrderingAmbiguous {
		t.Fatalf("bad description %+v", d)
	}
	if d.Selected.Key == "" || d.Selected.Revision == "" || d.Selected.Adapter == "" {
		t.Fatal("missing exact reference")
	}
	if _, e = s.Descriptions(ctx, []string{"other"}, key); e == nil || e.Error() != "not_found" {
		t.Fatal("scope escaped", e)
	}
	// Source disappearance preserves the independent archive projection.
	if e = s.SetHealth(ctx, r, "unavailable", testNow.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	retained, e := s.Descriptions(ctx, ns, key)
	if e != nil || retained.Selected.Text != d.Selected.Text {
		t.Fatal("source disappearance lost description", e)
	}
	// A late replay of historical input leaves native order, and therefore the
	// selected description, unchanged.
	if _, e = s.db.ExecContext(ctx, "UPDATE record_observations SET observed_at=? WHERE record_key=?", testNow.Add(24*time.Hour).Format(time.RFC3339Nano), d.PriorRecaps[0].Key); e != nil {
		t.Fatal(e)
	}
	replay, e := s.Descriptions(ctx, ns, key)
	if e != nil || replay.Selected.Key != d.Selected.Key {
		t.Fatal("arrival order changed description", e)
	}
	// Compression preserves compact candidate metadata and exact references.
	if _, e = s.CompressPayloads(ctx, 100, 16<<20); e != nil {
		t.Fatal(e)
	}
	compressed, e := s.Descriptions(ctx, ns, key)
	if e != nil || compressed.Selected.Key != d.Selected.Key || compressed.Selected.Text != d.Selected.Text {
		t.Fatal("compressed projection changed", e)
	}
}
func TestDescriptionsNewUserKeepsRecapAndClips(t *testing.T) {
	ctx := context.Background()
	s, r, _ := testStore(t)
	long := strings.Repeat("界", 3000)
	raw := []byte(`{"type":"system","subtype":"away_summary","uuid":"recap","sessionId":"synthetic-claude","content":"` + long + `"}` + "\n" + `{"type":"user","uuid":"u","sessionId":"synthetic-claude","message":{"role":"user","content":"do more"}}` + "\n")
	b := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, b)
	key := *b.Records[0].Record.ConversationKey
	ns := []string{r.Namespace}
	d, e := s.Descriptions(ctx, ns, key)
	if e != nil {
		t.Fatal(e)
	}
	if d.Kind != "native_recap" || !d.NewActivity || d.Selected == nil || !d.Selected.Truncated || len(d.Selected.Text) > 4096 {
		t.Fatalf("bad recap %+v", d)
	}
	exact, e := s.Get(ctx, ns, d.Selected.Key, d.Selected.Revision, d.Selected.Adapter, false)
	if e != nil || exact.Record.Body.Text != long {
		t.Fatal("clipping changed exact record", e)
	}
	if _, e = s.Redact(ctx, r.Namespace, "record", d.Selected.Key); e != nil {
		t.Fatal(e)
	}
	after, e := s.Descriptions(ctx, ns, key)
	if e != nil {
		t.Fatal(e)
	}
	if after.Kind != "unavailable" || after.Selected != nil {
		t.Fatal("redacted description survived", after)
	}
}
func TestDescriptionsCrossStreamAmbiguity(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	b := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, b)
	key := *b.Records[0].Record.ConversationKey
	second := r
	second.StreamID = "second"
	second.Path = "second.jsonl"
	if e := s.Register(ctx, []Registration{r, second}); e != nil {
		t.Fatal(e)
	}
	extra := []byte(`{"type":"assistant","uuid":"other","sessionId":"synthetic-claude","message":{"role":"assistant","content":"other stream answer"}}` + "\n")
	b2 := testBatch(t, extra, second, sessioncapture.Checkpoint{})
	mustIngest(t, s, second, sessioncapture.Checkpoint{}, b2)
	d, e := s.Descriptions(ctx, []string{r.Namespace}, key)
	if e != nil {
		t.Fatal(e)
	}
	if !d.OrderingAmbiguous {
		t.Fatal("invented causal cross-stream order", d)
	}
	again, e := s.Descriptions(ctx, []string{r.Namespace}, key)
	if e != nil || again.Selected.Key != d.Selected.Key {
		t.Fatal("nondeterministic selection", e)
	}
}
