package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/algorhythmic/skald/internal/secretredact"
	"github.com/algorhythmic/skald/internal/testsupport"
	"github.com/algorhythmic/skald/sessioncapture"
	"strings"
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
func TestLegacyDerivedTitleSanitizedBeforeClipping(t *testing.T) {
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	var target sessioncapture.Captured
	for _, item := range batch.Records {
		if item.Record.Kind == "message" && item.Record.Role != nil && *item.Record.Role == "user" {
			target = item
			break
		}
	}
	if target.Record.RecordKey == "" {
		t.Fatal("fixture missing user")
	}
	rec := target.Record
	value := strings.Repeat("Q", 18)
	rec.Body.Text = "password=\"" + value + "\""
	_, e := s.db.Exec(`UPDATE normalization_storage SET envelope_json=?,compressed_envelope=NULL,decoded_length=-1 WHERE record_key=?`, encode(rec), rec.RecordKey)
	if e != nil {
		t.Fatal("fixture setup")
	}
	if _, e = s.db.Exec(`DELETE FROM session_titles`); e != nil {
		t.Fatal("fixture setup")
	}
	if e = s.reprojectTitles(context.Background()); e != nil {
		t.Fatal("reprojection failed")
	}
	rows, e := s.db.Query(`SELECT title FROM session_titles`)
	if e != nil {
		t.Fatal("title query")
	}
	defer rows.Close()
	for rows.Next() {
		var title string
		if rows.Scan(&title) != nil || strings.Contains(title, value) {
			t.Fatal("legacy title leaked")
		}
	}
}
func TestEveryPatternCheckpointPrivacyStoredAndAcknowledged(t *testing.T) {
	for _, fixture := range testsupport.Secrets() {
		t.Run(fixture.Rule, func(t *testing.T) {
			s, r, _ := testStore(t)
			r.ConversationID = fixture.Input
			r.ProviderVersion = fixture.Input
			raw, _ := json.Marshal(map[string]any{"type": "user", "sessionId": fixture.Input, "uuid": "metadata-record", "version": fixture.Input, "message": map[string]string{"role": "user", "content": "ordinary synthetic body"}})
			raw = append(raw, '\n')
			batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
			d, e := sessioncapture.NewDelivery(r.Source, sessioncapture.Checkpoint{}, batch)
			if e != nil {
				t.Fatal("metadata delivery")
			}
			sink, e := s.CaptureSink("metadata-fixture", r)
			if e != nil {
				t.Fatal("metadata sink")
			}
			ack, e := sessioncapture.Deliver(context.Background(), sink, d)
			if e != nil {
				t.Fatal("metadata acceptance")
			}
			wire, _ := json.Marshal(ack)
			if secretredact.CheckWire(wire) != nil {
				t.Fatal("ack metadata leaked")
			}
			var stored string
			if s.db.QueryRow(`SELECT checkpoint_json FROM ingest_checkpoints WHERE stream_key=?`, r.Key()).Scan(&stored) != nil || secretredact.Check([]byte(stored)) != nil {
				t.Fatal("stored metadata leaked")
			}
			if _, e = sessioncapture.Deliver(context.Background(), sink, d); e != nil {
				t.Fatal("private metadata replay")
			}
		})
	}
}
