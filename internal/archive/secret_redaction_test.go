package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/algorhythmic/skald/internal/testsupport"
	"github.com/algorhythmic/skald/sessioncapture"
	"testing"
)

func TestSensitiveDirectIngestAdvancesSanitized(t *testing.T) {
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	batch.Records[0].Record.Extensions["password"] = json.RawMessage(`"synthetic"`)
	e := s.Ingest(context.Background(), r, sessioncapture.Checkpoint{}, batch, testNow)
	if e != nil {
		t.Fatal("sanitized direct ingest failed")
	}
	cp, e := s.Checkpoint(context.Background(), r.Key())
	if e != nil || cp.Offset != batch.Checkpoint.Offset {
		t.Fatal("checkpoint did not advance")
	}
}
func TestLegacySensitiveExactSanitized(t *testing.T) {
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
	if e != nil || len(exact.Raw) == 0 || exact.Record.Extensions["skald_redaction"] == nil {
		t.Fatal("legacy sensitive exact escaped")
	}
}

func TestEveryPatternStoredAdvancedExportedAndReplayed(t *testing.T) {
	for _, fixture := range testsupport.Secrets() {
		t.Run(fixture.Rule, func(t *testing.T) {
			s, r, _ := testStore(t)
			raw, _ := json.Marshal(map[string]any{"type": "user", "sessionId": "synthetic-claude", "uuid": "synthetic-record", "message": map[string]string{"role": "user", "content": fixture.Input}})
			raw = append(raw, '\n')
			batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
			if len(batch.Records) != 1 || batch.Checkpoint.Offset != int64(len(raw)) {
				t.Fatal("capture dropped or did not advance")
			}
			item := batch.Records[0]
			if item.Redaction == nil || item.Redaction.Counts[fixture.Rule] < 1 || bytes.Contains(item.Raw, []byte(fixture.Value)) {
				t.Fatal("sanitization metadata or payload wrong")
			}
			d, e := sessioncapture.NewDelivery(r.Source, sessioncapture.Checkpoint{}, batch)
			if e != nil {
				t.Fatal("delivery rejected")
			}
			sink, e := s.CaptureSink("synthetic-test", r)
			if e != nil {
				t.Fatal("sink unavailable")
			}
			if _, e = sessioncapture.Deliver(context.Background(), sink, d); e != nil {
				t.Fatal("sanitized delivery not stored")
			}
			if _, e = sessioncapture.Deliver(context.Background(), sink, d); e != nil {
				t.Fatal("sanitized replay not acknowledged")
			}
			cp, e := s.Checkpoint(context.Background(), r.Key())
			if e != nil || cp.Offset != int64(len(raw)) {
				t.Fatal("checkpoint not advanced")
			}
			exact, e := s.Get(context.Background(), []string{r.Namespace}, item.Record.RecordKey, item.Record.SourceRevision, item.Record.AdapterVersion, true)
			if e != nil || exact.Record.VerifyRaw(exact.Raw) != nil {
				t.Fatal("sanitized export invalid")
			}
			wire, _ := json.Marshal(exact)
			if bytes.Contains(wire, []byte(fixture.Value)) || bytes.Contains(exact.Raw, []byte(fixture.Value)) {
				t.Fatal("secret exported")
			}
			var stored []byte
			if e = s.db.QueryRow(`SELECT original_bytes FROM content_objects WHERE digest=?`, item.Record.SourceRevision).Scan(&stored); e != nil || bytes.Contains(stored, []byte(fixture.Value)) {
				t.Fatal("secret stored")
			}
			wire, _ = json.Marshal(d)
			if _, e = sessioncapture.DecodeDelivery(wire); e != nil {
				t.Fatal("transport replay invalid")
			}
		})
	}
}
