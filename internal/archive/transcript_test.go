package archive

import (
	"context"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
)

func TestTranscriptPagesScopeClippingAndProjects(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	// Include long Unicode and tool payloads to exercise independent display limits.
	text := strings.Repeat("界", 10000)
	raw = append(raw, []byte(`{"type":"assistant","uuid":"long","sessionId":"synthetic-claude","message":{"role":"assistant","content":[{"type":"text","text":"`+text+`"},{"type":"tool_use","id":"c","name":"Read","input":{"value":"`+text+`"}}]}}`+"\n")...)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	ns := []string{r.Namespace}
	sessions, err := s.Sessions(ctx, ns, "", 25)
	if err != nil {
		t.Fatal(err)
	}
	item := sessions.Items[0]
	if len(item.Projects) != 1 || item.Projects[0].Path != "/fixtures/skald" || item.Projects[0].Ref.RecordKey == "" {
		t.Fatalf("lost attributable project: %+v", item.Projects)
	}
	page, err := s.Transcript(ctx, ns, item.Key, "", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Next == "" || page.Items[0].Ordinal <= page.Items[1].Ordinal {
		t.Fatal("page order/bounds", page)
	}
	e := page.Items[0]
	if !e.Truncated || len(e.Text) > 16<<10 || len(e.Parts) != 1 || len(e.Parts[0].Text) > 16<<10 {
		t.Fatal("display limits not applied")
	}
	exact, err := s.Get(ctx, ns, e.Key, e.Revision, e.Adapter, false)
	if err != nil || exact.Record.Body.Text != text {
		t.Fatal("display clipping changed exact envelope", err)
	}
	second, err := s.Transcript(ctx, ns, item.Key, page.Next, 2, false)
	if err != nil || second.Boundary != page.Boundary || second.Items[0].Ordinal >= page.Items[1].Ordinal {
		t.Fatal("page repeated or boundary changed", err)
	}
	if _, err = s.Transcript(ctx, []string{"other"}, item.Key, "", 2, false); err == nil || err.Error() != "not_found" {
		t.Fatal("cross namespace read", err)
	}
	if _, err = s.Transcript(ctx, ns, item.Key, page.Next, 2, true); err == nil || err.Error() != "cursor_expired" {
		t.Fatal("history cursor was reused", err)
	}
	if _, err = s.Transcript(ctx, ns, item.Key, "", 26, false); err == nil {
		t.Fatal("unbounded page accepted")
	}
	// Rewrite a known native record; only the current normalization is shown by
	// default, while historical revisions remain explicitly addressable.
	changed := strings.Replace(string(raw), "I will check the archive boundaries.", "Revised answer.", 1)
	nextBatch := testBatch(t, []byte(changed), r, batch.Checkpoint)
	mustIngest(t, s, r, batch.Checkpoint, nextBatch)
	if _, err = s.Transcript(ctx, ns, item.Key, page.Next, 2, false); err == nil || err.Error() != "cursor_expired" {
		t.Fatal("stale boundary cursor accepted", err)
	}
	current, err := s.Transcript(ctx, ns, item.Key, "", 25, false)
	if err != nil {
		t.Fatal(err)
	}
	history, err := s.Transcript(ctx, ns, item.Key, "", 25, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Items) <= len(current.Items) {
		t.Fatal("history revision not retained")
	}
	for _, e := range current.Items {
		if !e.Current {
			t.Fatal("old revision in current view")
		}
	}
}
