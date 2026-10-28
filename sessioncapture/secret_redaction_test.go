package sessioncapture_test

import (
	"bytes"
	"encoding/json"
	"github.com/algorhythmic/skald/sessioncapture"
	"strings"
	"testing"
)

func TestSensitiveCaptureAdvancesSanitizedCheckpoint(t *testing.T) {
	for _, provider := range []string{sessioncapture.Claude, sessioncapture.Codex, sessioncapture.Devin} {
		t.Run(provider, func(t *testing.T) {
			s := source(provider)
			s.ConversationID = "synthetic"
			raw, _ := json.Marshal(map[string]any{"type": "unknown", "sessionId": "synthetic", "password": "syn" + "thetic-credential"})
			raw = append(raw, '\n')
			batch, e := sessioncapture.Read(bytes.NewReader(raw), s, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), clock)
			if e != nil || len(batch.Records) != 1 || batch.Checkpoint.Offset != int64(len(raw)) {
				t.Fatal("sanitized capture did not advance")
			}
			c := batch.Records[0]
			if c.Redaction == nil || bytes.Contains(c.Raw, []byte("synthetic-credential")) || c.Verify() != nil {
				t.Fatal("invalid sanitized capture")
			}
			d, e := sessioncapture.NewDelivery(s, sessioncapture.Checkpoint{}, batch)
			if e != nil || d.Version != 2 || d.Validate() != nil {
				t.Fatal("invalid sanitized replay")
			}
			wire, _ := json.Marshal(d)
			var instance any
			_ = json.Unmarshal(wire, &instance)
			if e := deliverySchemaPath(t, false, "../schemas/capture-delivery-v2.schema.json").Validate(instance); e != nil {
				t.Fatal("v2 schema validation failed")
			}
			if _, e = sessioncapture.DecodeDelivery(wire); e != nil {
				t.Fatal("sanitized transport rejected")
			}
		})
	}
}
func TestSanitizedAppendAndMetadataTamper(t *testing.T) {
	s := source(sessioncapture.Claude)
	s.ConversationID = "synthetic"
	raw, _ := json.Marshal(map[string]any{"type": "unknown", "sessionId": "synthetic", "password": "real" + "-synthetic-value"})
	raw = append(raw, '\n')
	first, e := sessioncapture.Read(bytes.NewReader(raw), s, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), clock)
	if e != nil {
		t.Fatal("initial capture")
	}
	tail := []byte(`{"type":"unknown","sessionId":"synthetic"}` + "\n")
	input := append(append([]byte{}, raw...), tail...)
	next, e := sessioncapture.Read(bytes.NewReader(input), s, first.Checkpoint, sessioncapture.DefaultLimits(), clock)
	if e != nil || next.Checkpoint.Offset != int64(len(input)) || len(next.Records) != 1 {
		t.Fatal("sanitized append continuity failed")
	}
	first.Records[0].Redaction.NativeBytes++
	if _, e = sessioncapture.NewDelivery(s, sessioncapture.Checkpoint{}, first); e == nil {
		t.Fatal("metadata tamper accepted")
	}
}

func TestQuotedAssignmentSuffixCaptureResumes(t *testing.T) {
	for _, quote := range []string{"\"", "'"} {
		s := source(sessioncapture.Claude)
		s.ConversationID = "synthetic"
		prefix := []byte(`{"type":"user","sessionId":"synthetic","uuid":"prefix","message":{"role":"user","content":"ordinary"}}` + "\n")
		first, err := sessioncapture.Read(bytes.NewReader(prefix), s, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), clock)
		if err != nil {
			t.Fatal("initial capture")
		}
		value := strings.Repeat("Q", 18)
		raw, _ := json.Marshal(map[string]any{"type": "user", "sessionId": "synthetic", "uuid": "tail", "message": map[string]string{"role": "user", "content": "password=" + quote + value + quote + ".suffix123"}})
		input := append(append([]byte{}, prefix...), append(raw, '\n')...)
		next, err := sessioncapture.Read(bytes.NewReader(input), s, first.Checkpoint, sessioncapture.DefaultLimits(), clock)
		if err != nil || len(next.Records) != 1 || next.Checkpoint.Offset != int64(len(input)) {
			t.Fatal("quoted assignment tail blocked capture")
		}
		if bytes.Contains(next.Records[0].Raw, []byte(value)) || next.Records[0].Verify() != nil {
			t.Fatal("sanitized tail invalid")
		}
		d, err := sessioncapture.NewDelivery(s, first.Checkpoint, next)
		if err != nil || d.Validate() != nil {
			t.Fatal("tail replay invalid")
		}
		final, err := sessioncapture.Read(bytes.NewReader(input), s, next.Checkpoint, sessioncapture.DefaultLimits(), clock)
		if err != nil || len(final.Records) != 0 {
			t.Fatal("tail replay duplicated")
		}
	}
}
