package sessioncapture_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

func TestRedactionFailureQuarantinesAndContinues(t *testing.T) {
	s := source(sessioncapture.Claude)
	s.ConversationID = "synthetic"
	hidden := strings.Repeat("Q", 18)
	bad := []byte(`{"type":"user","sessionId":"synthetic","uuid":"bad","message":{"role":"user","content":"` + hidden + `"},"nested":` + strings.Repeat("[", 140) + `0` + strings.Repeat("]", 140) + "}\n")
	good := []byte(`{"type":"user","sessionId":"synthetic","uuid":"good","message":{"role":"user","content":"ordinary"}}` + "\n")
	input := append(append([]byte{}, bad...), good...)
	batch, err := sessioncapture.Read(bytes.NewReader(input), s, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), clock)
	if err != nil || len(batch.Records) != 2 || batch.Checkpoint.Offset != int64(len(input)) || batch.Checkpoint.Ordinal != 2 || batch.Blocked {
		t.Fatal("redaction failure stalled stream")
	}
	c := batch.Records[0]
	q, ok := c.QuarantineMetadata()
	if !ok || q.Ordinal != 0 || q.SourceOffset != 0 || q.Reason != sessioncapture.QuarantineReason || q.PatternID != sessioncapture.QuarantinePattern || c.NativeLength() != int64(len(bad)) {
		t.Fatal("reprocess metadata missing")
	}
	wire, _ := json.Marshal(c)
	if bytes.Contains(wire, []byte(hidden)) || c.Record.Body.Text != "" || len(c.Record.Body.Parts) != 0 || bytes.Contains(c.Raw, []byte("nested")) || c.Verify() != nil {
		t.Fatal("quarantine retained content")
	}
	if len(batch.Gaps) != 1 || batch.Gaps[0].Code != sessioncapture.QuarantineReason || !batch.Checkpoint.HasGaps || batch.Checkpoint.ParsedOffset != 0 {
		t.Fatal("quarantine coverage missing")
	}
	d, err := sessioncapture.NewDelivery(s, sessioncapture.Checkpoint{}, batch)
	if err != nil || d.Validate() != nil {
		t.Fatal("quarantine delivery invalid")
	}
	wire, _ = json.Marshal(d)
	var instance any
	json.Unmarshal(wire, &instance)
	if deliverySchemaPath(t, false, "../schemas/capture-delivery-v2.schema.json").Validate(instance) != nil {
		t.Fatal("quarantine schema invalid")
	}

	next, err := sessioncapture.Read(bytes.NewReader(input), s, batch.Checkpoint, sessioncapture.DefaultLimits(), clock)
	if err != nil || len(next.Records) != 0 {
		t.Fatal("quarantine checkpoint replay duplicated")
	}
}

func TestDirectBatchRedactionFailureQuarantined(t *testing.T) {
	s := source(sessioncapture.Claude)
	s.ConversationID = "synthetic"
	clean := []byte(`{"type":"user","sessionId":"synthetic","message":{"role":"user","content":"ordinary"}}` + "\n")
	batch, err := sessioncapture.Read(bytes.NewReader(clean), s, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), clock)
	if err != nil {
		t.Fatal("fixture capture")
	}
	raw := []byte(`{"nested":` + strings.Repeat("[", 140) + `0` + strings.Repeat("]", 140) + "}\n")
	c := batch.Records[0]
	c.Raw = raw
	c.Record.SourceRevision = sessionrecord.Digest(raw)
	c.Record.RawRef = &sessionrecord.Ref{RecordKey: c.Record.RecordKey, SourceRevision: c.Record.SourceRevision}
	batch.Records[0] = c
	batch.Checkpoint.Offset = int64(len(raw))
	batch.Checkpoint.ParsedOffset = int64(len(raw))
	batch.Checkpoint.PrefixDigest = sessionrecord.Digest(raw)
	safe, err := sessioncapture.SanitizeBatch(batch)
	if err != nil || len(safe.Records) != 1 || !safe.Checkpoint.HasGaps || len(safe.Gaps) != 1 || safe.Records[0].Verify() != nil {
		t.Fatal("direct ingest batch not quarantined")
	}
	if _, ok := safe.Records[0].QuarantineMetadata(); !ok {
		t.Fatal("quarantine metadata missing")
	}
	tampered := safe.Records[0]
	tampered.Record.Body.Text = "unexpected fixture content"
	if tampered.Verify() == nil {
		t.Fatal("quarantine accepted injected content")
	}
}
