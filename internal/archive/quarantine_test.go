package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
)

func TestQuarantineStoredCountAlertAndReplay(t *testing.T) {
	ctx := context.Background()
	s, r, _ := testStore(t)
	hidden := strings.Repeat("Q", 18)
	bad := []byte(`{"type":"user","sessionId":"synthetic-claude","uuid":"bad","message":{"role":"user","content":"` + hidden + `"},"nested":` + strings.Repeat("[", 140) + `0` + strings.Repeat("]", 140) + "}\n")
	good := []byte(`{"type":"user","sessionId":"synthetic-claude","uuid":"good","message":{"role":"user","content":"ordinary"}}` + "\n")
	raw := append(append([]byte{}, bad...), good...)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	if len(batch.Records) != 2 {
		t.Fatal("quarantine stopped capture")
	}
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	cp, err := s.Checkpoint(ctx, r.Key())
	if err != nil || cp.Offset != int64(len(raw)) || cp.Ordinal != 2 {
		t.Fatal("checkpoint did not advance")
	}
	var wire string
	if s.db.QueryRow(`SELECT envelope_json FROM normalizations WHERE record_key=?`, batch.Records[0].Record.RecordKey).Scan(&wire) != nil || strings.Contains(wire, hidden) {
		t.Fatal("quarantine stored content")
	}
	var payload []byte
	if s.db.QueryRow(`SELECT original_bytes FROM content_objects WHERE digest=?`, batch.Records[0].Record.SourceRevision).Scan(&payload) != nil || bytes.Contains(payload, []byte(hidden)) {
		t.Fatal("native content retained")
	}
	var metadata sessioncapture.Quarantine
	if json.Unmarshal(payload, &metadata) != nil || metadata.Ordinal != 0 || metadata.SourceOffset != 0 || metadata.Reason != sessioncapture.QuarantineReason || metadata.PatternID != sessioncapture.QuarantinePattern {
		t.Fatal("reprocessing metadata missing")
	}
	for _, tc := range []struct {
		at          time.Time
		total, hour int64
		health      string
	}{
		{testNow, 1, 1, "degraded"}, {testNow.Add(time.Hour), 1, 1, "degraded"}, {testNow.Add(time.Hour + time.Second), 1, 0, "healthy"},
	} {
		status, err := s.quarantineStatus(ctx, []string{r.Namespace}, tc.at)
		if err != nil || status.Total != tc.total || status.LastHour != tc.hour || status.Health != tc.health {
			t.Fatal("quarantine alert/count mismatch")
		}
		if tc.hour > 0 && status.Reason != sessioncapture.QuarantineReason {
			t.Fatal("alert reason missing")
		}
	}
	foreign, err := s.quarantineStatus(ctx, []string{"fixture:foreign"}, testNow)
	if err != nil || foreign.Total != 0 || foreign.LastHour != 0 || foreign.Health != "healthy" {
		t.Fatal("quarantine count escaped scope")
	}
	next := testBatch(t, raw, r, cp)
	mustIngest(t, s, r, cp, next)
	status, err := s.quarantineStatus(ctx, []string{r.Namespace}, testNow.Add(time.Minute))
	if err != nil || status.Total != 1 {
		t.Fatal("quarantine replay duplicated")
	}
	full, err := s.Status(ctx, []string{r.Namespace})
	if err != nil || full["quarantine"].(QuarantineStatus).Total != 1 {
		t.Fatal("status count missing")
	}
}
