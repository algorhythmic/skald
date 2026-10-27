package sessioncapture_test

import (
	"bytes"
	"encoding/json"
	"github.com/algorhythmic/skald/sessioncapture"
	"testing"
)

func TestSensitiveCaptureFailsBeforeCheckpoint(t *testing.T) {
	for _, provider := range []string{sessioncapture.Claude, sessioncapture.Codex, sessioncapture.Devin} {
		t.Run(provider, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"type": "user", "password": "synthetic"})
			raw = append(raw, '\n')
			batch, e := sessioncapture.Read(bytes.NewReader(raw), source(provider), sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), clock)
			if e == nil || e.Error() != "secret_redaction_required" || len(batch.Records) != 0 || batch.Checkpoint.Offset != 0 {
				t.Fatal("sensitive bytes escaped capture gate")
			}
		})
	}
}
