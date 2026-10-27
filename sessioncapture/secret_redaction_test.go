package sessioncapture_test

import (
	"bytes"
	"encoding/json"
	"github.com/algorhythmic/skald/sessioncapture"
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
			if _, e = sessioncapture.DecodeDelivery(wire); e != nil {
				t.Fatal("sanitized transport rejected")
			}
		})
	}
}
