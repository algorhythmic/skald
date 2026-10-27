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
func TestLegacySensitiveExactWithheld(t *testing.T) {
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	item := batch.Records[0]
	rec := item.Record
	rec.Extensions = map[string]json.RawMessage{"password": json.RawMessage(`"synthetic"`)}
	_, e := s.db.Exec(`UPDATE normalization_storage SET envelope_json=?,compressed_envelope=NULL,decoded_length=-1 WHERE record_key=?`, encode(rec), rec.RecordKey)
	if e != nil {
		t.Fatal("legacy fixture setup")
	}
	exact, e := s.Get(context.Background(), []string{r.Namespace}, rec.RecordKey, rec.SourceRevision, rec.AdapterVersion, true)
	if e == nil || e.Error() != "secret_redaction_required" || len(exact.Raw) != 0 {
		t.Fatal("legacy sensitive exact escaped")
	}
}
