package archive

import (
	"context"
	"encoding/json"
	"github.com/algorhythmic/skald/sessioncapture"
	"testing"
)

func TestSensitiveDirectIngestDoesNotAdvance(t *testing.T) {
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	batch.Records[0].Record.Extensions["password"] = json.RawMessage(`"synthetic"`)
	e := s.Ingest(context.Background(), r, sessioncapture.Checkpoint{}, batch, testNow)
	if e == nil || e.Error() != "secret_redaction_required" {
		t.Fatal("sensitive direct ingest allowed")
	}
	cp, e := s.Checkpoint(context.Background(), r.Key())
	if e != nil || cp.Offset != 0 {
		t.Fatal("checkpoint advanced")
	}
}
