package archive

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
)

func TestPayloadCompressionRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	var compressed int
	if err := s.db.QueryRow("SELECT count(*) FROM normalization_storage WHERE compressed_envelope IS NOT NULL").Scan(&compressed); err != nil || compressed == 0 {
		t.Fatal("new envelopes were not compressed", compressed, err)
	}
	// A complete synthetic envelope remains byte-for-byte identical through views.
	var before string
	if err := s.db.QueryRow("SELECT envelope_json FROM normalizations LIMIT 1").Scan(&before); err != nil {
		t.Fatal(err)
	}
	total := CompressionResult{}
	for i := 0; i < 10; i++ {
		got, err := s.CompressPayloads(ctx, 100, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		total.Compressed += got.Compressed
		total.Examined += got.Examined
		if got.Examined == 0 {
			break
		}
	}
	if total.Examined != 0 || total.Compressed != 0 {
		t.Fatal("new ingest left pending compression work", total)
	}
	for table, index := range map[string]string{"content_storage": "content_pending_compression", "normalization_storage": "normalization_pending_compression"} {
		var pending int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table + " INDEXED BY " + index + " WHERE decoded_length IS NULL").Scan(&pending); err != nil || pending != 0 {
			t.Fatal(table, pending, err)
		}
		plan := queryPlan(t, s.db, "SELECT rowid FROM "+table+" INDEXED BY "+index+" WHERE decoded_length IS NULL LIMIT 100")
		if !strings.Contains(plan, index) {
			t.Fatal("pending index not used", plan)
		}
	}

	var after string
	if err := s.db.QueryRow("SELECT envelope_json FROM normalizations LIMIT 1").Scan(&after); err != nil || before != after {
		t.Fatal("envelope changed", err)
	}
	for _, item := range batch.Records {
		got, err := s.Get(ctx, []string{r.Namespace}, item.Record.RecordKey, item.Record.SourceRevision, item.Record.AdapterVersion, true)
		if err != nil || !bytes.Equal(got.Raw, item.Raw) {
			t.Fatal("raw changed", err)
		}
	}
	backup, err := s.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err = Restore(ctx, backup, target, DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(target, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err = restored.db.QueryRow("SELECT envelope_json FROM normalizations LIMIT 1").Scan(&after); err != nil || before != after {
		t.Fatal("reopen changed envelope", err)
	}
	var files []string
	files, _ = filepath.Glob(filepath.Join(target, "pre-schema-8-*.sqlite"))
	if len(files) != 0 {
		t.Fatal("current schema remigrated")
	}
}

// Restore the schema-8 representation of a legacy row awaiting explicit
// maintenance. Public views must work alongside newly compressed append data.
func legacyPayload(t *testing.T, s *Store, item sessioncapture.Captured) {
	t.Helper()
	if _, err := s.db.Exec("UPDATE content_storage SET original_bytes=?,byte_length=?,decoded_length=NULL WHERE digest=?", item.Raw, len(item.Raw), item.Record.SourceRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE normalization_storage SET envelope_json=?,compressed_envelope=NULL,decoded_length=NULL,envelope_digest=NULL,display_role=NULL,display_usable=NULL WHERE record_key=? AND source_revision=? AND adapter_version=?", encode(item.Record), item.Record.RecordKey, item.Record.SourceRevision, item.Record.AdapterVersion); err != nil {
		t.Fatal(err)
	}
}

func TestIngestCompressionMixedLegacyAndAppend(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	first := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, first)
	legacyPayload(t, s, first.Records[0])
	dir := s.dir
	s.Close()
	s, err := Open(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var pending int
	if err := s.db.QueryRow("SELECT count(*) FROM content_storage WHERE decoded_length IS NULL").Scan(&pending); err != nil || pending != 1 {
		t.Fatal("reopen converted legacy content", pending, err)
	}
	text := strings.Repeat("A compressible new answer. ", 64)
	appended := append(append([]byte{}, raw...), []byte("{}\n"+`{"type":"assistant","uuid":"compressed-append","sessionId":"synthetic-claude","message":{"role":"assistant","content":"`+text+`"}}`+"\n")...)
	second := testBatch(t, appended, r, first.Checkpoint)
	mustIngest(t, s, r, first.Checkpoint, second)
	if len(second.Records) != 2 {
		t.Fatal("append did not retain both rows", len(second.Records))
	}
	var plain, packed int64
	if err := s.db.QueryRow("SELECT decoded_length FROM content_storage WHERE digest=?", second.Records[0].Record.SourceRevision).Scan(&plain); err != nil || plain != -1 {
		t.Fatal("tiny content did not stay plain", plain, err)
	}
	if err := s.db.QueryRow("SELECT decoded_length FROM content_storage WHERE digest=?", second.Records[1].Record.SourceRevision).Scan(&packed); err != nil || packed != int64(len(second.Records[1].Raw)) {
		t.Fatal("new content was not compressed", packed, err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM content_storage WHERE decoded_length IS NULL").Scan(&pending); err != nil || pending != 1 {
		t.Fatal("append converted legacy content", pending, err)
	}
	all := append(append([]sessioncapture.Captured{}, first.Records...), second.Records...)
	ns, conversation := []string{r.Namespace}, *first.Records[0].Record.ConversationKey
	for _, item := range all {
		exact, err := s.Get(ctx, ns, item.Record.RecordKey, item.Record.SourceRevision, item.Record.AdapterVersion, true)
		if err != nil || !bytes.Equal(exact.Raw, item.Raw) || encode(exact.Record) != encode(item.Record) {
			t.Fatal("mixed exact read changed evidence", err)
		}
	}
	transcript, err := s.Transcript(ctx, ns, conversation, "", 25, false)
	if err != nil {
		t.Fatal(err)
	}
	description, err := s.Descriptions(ctx, ns, conversation)
	if err != nil || description.Selected == nil || description.Selected.Text != text {
		t.Fatal("compressed append display metadata missing", description, err)
	}
	if err := s.SetReadScope(ctx, ns); err != nil {
		t.Fatal(err)
	}
	generation, err := s.ReadScopeGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retrieval, err := s.RetrievalSnapshot(ctx, ns, generation, 1000, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range all {
		legacyPayload(t, s, item)
	}
	plainTranscript, err := s.Transcript(ctx, ns, conversation, "", 25, false)
	if err != nil || !reflect.DeepEqual(transcript, plainTranscript) {
		t.Fatal("mixed transcript differs from plain evidence", err)
	}
	plainDescription, err := s.Descriptions(ctx, ns, conversation)
	if err != nil || !reflect.DeepEqual(description, plainDescription) {
		t.Fatal("mixed descriptions differ from plain evidence", err)
	}
	plainRetrieval, err := s.RetrievalSnapshot(ctx, ns, generation, 1000, 8<<20)
	if err != nil || !reflect.DeepEqual(retrieval, plainRetrieval) {
		t.Fatal("mixed retrieval differs from plain evidence", err)
	}
	maintenance, err := s.CompressPayloads(ctx, 100, 8<<20)
	if err != nil || maintenance.Compressed == 0 {
		t.Fatal("explicit legacy maintenance stopped working", maintenance, err)
	}
}
func TestCompressedNormalizationCorruptionRejected(t *testing.T) {
	s, r, raw := testStore(t)
	ctx := context.Background()
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	if _, err := s.CompressPayloads(ctx, 100, 64<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE normalization_storage SET envelope_digest=? WHERE compressed_envelope IS NOT NULL", strings.Repeat("x", 71)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(ctx); err == nil {
		t.Fatal("corrupt normalization backup accepted")
	}
}
func TestCompressedPayloadRedaction(t *testing.T) {
	s, r, raw := testStore(t)
	ctx := context.Background()
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	if _, err := s.CompressPayloads(ctx, 100, 64<<20); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.Redact(ctx, r.Namespace, "source", r.Namespace)
	if err != nil || receipt.BytesRemoved == 0 {
		t.Fatal(receipt, err)
	}
	for _, table := range []string{"content_storage", "normalization_storage"} {
		var n int
		if err = s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal(table, n, err)
		}
	}
}

func TestInflatePayloadBoundsAndCorruption(t *testing.T) {
	raw := []byte(strings.Repeat("test", 10000))
	packed, err := deflatePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int64{-1, maxPayloadBytes + 1, int64(len(raw) - 1), int64(len(raw) + 1)} {
		if _, err = inflatePayload(packed, n); err == nil {
			t.Fatalf("accepted length %d", n)
		}
	}
	if _, err = inflatePayload(append(packed, 1), int64(len(raw))); err == nil {
		t.Fatal("accepted trailing bytes")
	}
	packed[len(packed)/2] ^= 128
	if _, err = inflatePayload(packed, int64(len(raw))); err == nil {
		t.Fatal("accepted corruption")
	}
}
func TestPayloadCompressionLimits(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.CompressPayloads(ctx, 0, 1); err == nil {
		t.Fatal("invalid rows accepted")
	}
	s.options.MinFreeBytes = ^uint64(0)
	if _, err := s.CompressPayloads(ctx, 1, 1<<20); err != ErrCapacity {
		t.Fatal(err)
	}
}
func TestSchemaSevenCompressionMigration(t *testing.T) {
	dir := schemaSixFixture(t)
	db, err := sql.Open("sqlite", DSN(filepath.Join(dir, "archive.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	ddl, _ := migrations.ReadFile("migrations/007_read_indexes.sql")
	if _, err = db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	copies, _ := filepath.Glob(filepath.Join(dir, "pre-schema-8-*.sqlite"))
	if len(copies) != 1 {
		t.Fatal("missing rollback", copies)
	}
	if err = verifyFile(context.Background(), copies[0], false); err != nil {
		t.Fatal(err)
	}
	var length int
	if err = s.db.QueryRow("SELECT byte_length FROM content_objects").Scan(&length); err != nil || length != 1 {
		t.Fatal(length, err)
	}
	if _, err = s.CompressPayloads(context.Background(), 100, 1<<20); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var raw []byte
	if err = s.db.QueryRow("SELECT original_bytes FROM content_objects").Scan(&raw); err != nil || string(raw) != "a" {
		t.Fatal(string(raw), err)
	}
	restored := filepath.Join(t.TempDir(), "old-restore")
	if err = Restore(context.Background(), copies[0], restored, DefaultOptions()); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkPayloadCodec(b *testing.B) {
	raw := []byte(strings.Repeat(`{"type":"message","content":"repeated synthetic conversation payload"}`+"\n", 1024))
	packed, _ := deflatePayload(raw)
	b.ReportMetric(float64(len(packed))/float64(len(raw))*100, "stored_percent")
	b.Run("compress", func(b *testing.B) {
		b.ReportMetric(float64(len(packed))/float64(len(raw))*100, "stored_percent")
		b.SetBytes(int64(len(raw)))
		for i := 0; i < b.N; i++ {
			deflatePayload(raw)
		}
	})
	b.Run("decode", func(b *testing.B) {
		b.ReportMetric(float64(len(packed))/float64(len(raw))*100, "stored_percent")
		b.SetBytes(int64(len(raw)))
		for i := 0; i < b.N; i++ {
			inflatePayload(packed, int64(len(raw)))
		}
	})
}

// The fresh encoder represents the previous implementation's per-row cost.
func BenchmarkPayloadSmallBatch(b *testing.B) {
	raw := []byte(`{"type":"assistant","sessionId":"synthetic","message":{"role":"assistant","content":"` + strings.Repeat("example conversation text ", 75) + `"}}`)
	for name, encode := range map[string]func([]byte) ([]byte, error){
		"fresh_writer": func(raw []byte) ([]byte, error) {
			var out bytes.Buffer
			w, _ := zlib.NewWriterLevel(&out, zlib.BestSpeed)
			_, err := w.Write(raw)
			if err != nil {
				return nil, err
			}
			if err = w.Close(); err != nil {
				return nil, err
			}
			return out.Bytes(), nil
		},
		"pooled_writer": deflatePayload,
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw) * 100))
			b.ReportMetric(100, "rows/op")
			for i := 0; i < b.N; i++ {
				for row := 0; row < 100; row++ {
					if _, err := encode(raw); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
