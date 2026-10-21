package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

func TestRedactRecordAtomicIdempotentAndScoped(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	target := batch.Records[0].Record.RecordKey
	if _, err := s.Redact(ctx, r.Namespace, "source", "wrong"); err == nil || err.Error() != "redaction_scope_mismatch" {
		t.Fatal("source target widened", err)
	}
	if _, err := s.Redact(ctx, "another-namespace", "record", target); err == nil || err.Error() != "redaction_target_unavailable" {
		t.Fatal("cross-namespace record redacted", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_withdrawal BEFORE INSERT ON changes
 WHEN NEW.change_kind='withdrawal' BEGIN SELECT RAISE(ABORT,'injected withdrawal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Redact(ctx, r.Namespace, "record", target); err == nil {
		t.Fatal("injected withdrawal failure ignored")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM artifact_versions WHERE record_key=?", target).Scan(&count); err != nil || count == 0 {
		t.Fatal("failed redaction partially removed content", count, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM purge_tombstones WHERE namespace=?", r.Namespace).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed redaction published tombstone", count, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_withdrawal"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_receipt BEFORE INSERT ON redaction_receipts
 BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Redact(ctx, r.Namespace, "record", target); err == nil {
		t.Fatal("injected receipt failure ignored")
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM artifact_versions WHERE record_key=?", target).Scan(&count); err != nil || count == 0 {
		t.Fatal("failed receipt partially removed content", count, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM purge_tombstones WHERE namespace=?", r.Namespace).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed receipt published tombstone", count, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM changes WHERE change_kind='withdrawal' AND namespace=?", r.Namespace).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed receipt published withdrawal", count, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_receipt"); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.Redact(ctx, r.Namespace, "record", target)
	if err != nil || receipt.Sequence == 0 || receipt.RecordsRemoved != 1 || receipt.VersionsRemoved != 1 || receipt.ObjectsRemoved != 1 || receipt.BytesRemoved != int64(len(batch.Records[0].Raw)) {
		t.Fatal("bad record receipt", receipt, err)
	}
	schemaBytes, err := os.ReadFile("../../schemas/redaction-receipt-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	var instance any
	if err := json.Unmarshal([]byte(encode(receipt)), &instance); err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(instance); err != nil {
		t.Fatal("receipt violates shared schema", err)
	}
	fixture, err := os.ReadFile("../../testdata/redaction-receipt/receipt-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture, &instance); err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(instance); err != nil {
		t.Fatal("synthetic receipt violates shared schema", err)
	}
	again, err := s.Redact(ctx, r.Namespace, "record", target)
	if err != nil || !reflect.DeepEqual(again, receipt) {
		t.Fatal("idempotent receipt changed", again, err)
	}
	if _, err := s.Get(ctx, []string{r.Namespace}, target, batch.Records[0].Record.SourceRevision, sessioncapture.AdapterVersion, true); err == nil {
		t.Fatal("redacted exact content still readable")
	}
	for _, query := range []string{
		"SELECT count(*) FROM artifact_versions WHERE record_key=?",
		"SELECT count(*) FROM normalizations WHERE record_key=?",
		"SELECT count(*) FROM record_observations WHERE record_key=?",
		"SELECT count(*) FROM record_heads WHERE record_key=?",
		"SELECT count(*) FROM session_titles WHERE record_key=?",
		"SELECT count(*) FROM session_projects WHERE record_key=?",
	} {
		if err := s.db.QueryRowContext(ctx, query, target).Scan(&count); err != nil || count != 0 {
			t.Fatal("redacted projection survived", query, count, err)
		}
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM artifacts WHERE record_key=?", target).Scan(&count); err != nil || count != 1 {
		t.Fatal("record identity lost", count, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM changes WHERE sequence=? AND change_kind='withdrawal'", receipt.Sequence).Scan(&count); err != nil || count != 1 {
		t.Fatal("withdrawal missing", count, err)
	}
	page, err := s.Changes(ctx, []string{r.Namespace}, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var unavailable, withdrawal bool
	for _, event := range page.Events {
		if event.RecordKey == target && event.Kind == "record" {
			unavailable = event.Unavailable == "redacted" && event.Locator == nil
		}
		if event.Sequence == receipt.Sequence {
			withdrawal = event.Kind == "redaction" && event.ScopeKind == "record" && event.ScopeKey == target
		}
	}
	if !unavailable || !withdrawal {
		t.Fatal("feed did not preserve metadata while withdrawing exact evidence")
	}
}

func TestRedactConversationBackupRestoreAndSuppression(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	conversation := sessionrecord.Conversation(r.Namespace, batch.Checkpoint.ConversationID)
	receipt, err := s.Redact(ctx, r.Namespace, "conversation", conversation)
	if err != nil || receipt.RecordsRemoved != int64(len(batch.Records)) {
		t.Fatal("conversation redaction failed", receipt, err)
	}
	var versions int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM artifact_versions").Scan(&versions); err != nil || versions != 0 {
		t.Fatal("conversation content survived", versions, err)
	}
	backup, err := s.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if err := Restore(ctx, backup, destination, DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(destination, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var objects int
	if err := restored.db.QueryRowContext(ctx, "SELECT count(*) FROM content_objects").Scan(&objects); err != nil || objects != 0 {
		t.Fatal("restored backup retained redacted content", objects, err)
	}
	again, err := restored.Redact(ctx, r.Namespace, "conversation", conversation)
	if err != nil || !reflect.DeepEqual(again, receipt) {
		t.Fatal("restored receipt changed", again, err)
	}
	appended := append(append([]byte{}, raw...), []byte(`{"type":"assistant","sessionId":"synthetic-claude","message":{"role":"assistant","content":"synthetic continuation"}}`+"\n")...)
	tail := testBatch(t, appended, r, batch.Checkpoint)
	if err := restored.Ingest(ctx, r, batch.Checkpoint, tail, testNow); err == nil || err.Error() != "capture_suppressed" {
		t.Fatal("restored tombstone allowed reingest", err)
	}
}

func TestRedactSourceKeepsSharedDigestForOtherNamespace(t *testing.T) {
	ctx := context.Background()
	s, first, raw := testStore(t)
	firstBatch := testBatch(t, raw, first, sessioncapture.Checkpoint{})
	mustIngest(t, s, first, sessioncapture.Checkpoint{}, firstBatch)
	second := Registration{Source: sessioncapture.Source{Namespace: "fixture:second", Provider: sessioncapture.Claude, StreamID: "other"}, Root: t.TempDir(), Path: "second.jsonl"}
	if err := s.Register(ctx, []Registration{first, second}); err != nil {
		t.Fatal(err)
	}
	secondBatch := testBatch(t, raw, second, sessioncapture.Checkpoint{})
	mustIngest(t, s, second, sessioncapture.Checkpoint{}, secondBatch)
	receipt, err := s.Redact(ctx, first.Namespace, "source", first.Namespace)
	if err != nil || receipt.RecordsRemoved != int64(len(firstBatch.Records)) || receipt.ObjectsRemoved != 0 {
		t.Fatal("shared content incorrectly deleted", receipt, err)
	}
	record := secondBatch.Records[0]
	exact, err := s.Get(ctx, []string{second.Namespace}, record.Record.RecordKey, record.Record.SourceRevision, record.Record.AdapterVersion, true)
	if err != nil || !bytes.Equal(exact.Raw, record.Raw) {
		t.Fatal("other namespace lost shared source bytes", err)
	}
	if _, err := s.Get(ctx, []string{first.Namespace}, firstBatch.Records[0].Record.RecordKey, firstBatch.Records[0].Record.SourceRevision, firstBatch.Records[0].Record.AdapterVersion, true); err == nil {
		t.Fatal("source redaction exposed old exact content")
	}
	tail := testBatch(t, append(append([]byte{}, raw...), []byte(`{"type":"assistant","sessionId":"synthetic-claude","message":{"role":"assistant","content":"next"}}`+"\n")...), first, firstBatch.Checkpoint)
	if err := s.Ingest(ctx, first, firstBatch.Checkpoint, tail, testNow); err == nil || err.Error() != "capture_suppressed" {
		t.Fatal("source tombstone allowed reingest", err)
	}
}
