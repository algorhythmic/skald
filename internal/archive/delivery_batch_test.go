package archive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
)

const batchDestination = "synthetic:grouped"

func batchFixture(t *testing.T) (*Store, []Registration, []sessioncapture.Delivery, []byte) {
	t.Helper()
	s, r, raw := testStore(t)
	other := r
	other.Namespace = "fixture:other"
	registrations := []Registration{r, other}
	if err := s.Register(context.Background(), registrations); err != nil {
		t.Fatal(err)
	}
	firstLine := raw[:bytes.IndexByte(raw, '\n')+1]
	deliveries := make([]sessioncapture.Delivery, len(registrations))
	for i, r := range registrations {
		batch := testBatch(t, firstLine, r, sessioncapture.Checkpoint{})
		var err error
		deliveries[i], err = sessioncapture.NewDelivery(r.Source, sessioncapture.Checkpoint{}, batch)
		if err != nil {
			t.Fatal(err)
		}
	}
	return s, registrations, deliveries, raw
}

func TestCaptureBatchRollbackRetryAndReopen(t *testing.T) {
	ctx := context.Background()
	s, registrations, deliveries, raw := batchFixture(t)
	for _, r := range registrations {
		if err := s.SetHealth(ctx, r, "unavailable", testNow); err != nil {
			t.Fatal(err)
		}
	}
	beforeHealth := maps.Clone(s.healthChecks)
	before, err := s.boundary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_second_receipt BEFORE INSERT ON capture_receipts WHEN NEW.source_key='%s' BEGIN SELECT RAISE(ABORT,'synthetic grouped receipt failure'); END`, registrations[1].Key())); err != nil {
		t.Fatal(err)
	}
	if acks, err := s.AcceptCaptureBatch(ctx, batchDestination, registrations, deliveries); err == nil || acks != nil {
		t.Fatal("partial group acknowledged", acks, err)
	}
	if !reflect.DeepEqual(beforeHealth, s.healthChecks) {
		t.Fatal("rolled back group advanced health cache")
	}
	for _, r := range registrations {
		cp, err := s.Checkpoint(ctx, r.Key())
		if err != nil || cp != (sessioncapture.Checkpoint{}) {
			t.Fatal("partial group checkpoint survived", cp, err)
		}
	}
	for _, table := range []string{"content_objects", "artifact_versions", "record_observations", "ingest_checkpoints", "capture_receipts"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s retained %d rows: %v", table, count, err)
		}
	}
	if boundary, err := s.boundary(); err != nil || boundary != before {
		t.Fatal("rollback published changes", boundary, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_second_receipt"); err != nil {
		t.Fatal(err)
	}
	acks, err := s.AcceptCaptureBatch(ctx, batchDestination, registrations, deliveries)
	if err != nil || len(acks) != len(deliveries) {
		t.Fatal("retry group", acks, err)
	}
	for i, ack := range acks {
		if err := ack.Validate(batchDestination, deliveries[i]); err != nil {
			t.Fatal(err)
		}
		if s.healthChecks[registrations[i].Key()].health != "available" {
			t.Fatal("committed group did not publish health")
		}
	}
	tail := testBatch(t, raw, registrations[0], deliveries[0].Batch.Checkpoint)
	next, err := sessioncapture.NewDelivery(registrations[0].Source, deliveries[0].Batch.Checkpoint, tail)
	if err != nil {
		t.Fatal(err)
	}
	mixedRegistrations := []Registration{registrations[0], registrations[0], registrations[1]}
	mixedDeliveries := []sessioncapture.Delivery{deliveries[0], next, deliveries[1]}
	mixed, err := s.AcceptCaptureBatch(ctx, batchDestination, mixedRegistrations, mixedDeliveries)
	if err != nil || mixed[0] != acks[0] || mixed[2] != acks[1] {
		t.Fatal("mixed receipt replay/new evidence", mixed, err)
	}
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replayed, err := reopened.AcceptCaptureBatch(ctx, batchDestination, mixedRegistrations, mixedDeliveries)
	if err != nil || !reflect.DeepEqual(replayed, mixed) {
		t.Fatal("group receipt replay after restart", replayed, err)
	}
	if cp, err := reopened.Checkpoint(ctx, registrations[0].Key()); err != nil || cp != next.Batch.Checkpoint {
		t.Fatal("old receipt regressed checkpoint", cp, err)
	}
	if len(reopened.healthChecks) != 0 {
		t.Fatal("receipt replay fabricated fresh health observation")
	}
}

func TestCaptureBatchOrdersSourceChainsAndRejectsSuppression(t *testing.T) {
	ctx := context.Background()
	s, registrations, deliveries, raw := batchFixture(t)
	tail := testBatch(t, raw, registrations[0], deliveries[0].Batch.Checkpoint)
	next, err := sessioncapture.NewDelivery(registrations[0].Source, deliveries[0].Batch.Checkpoint, tail)
	if err != nil {
		t.Fatal(err)
	}
	sameSource := []Registration{registrations[0], registrations[0]}
	if acks, err := s.AcceptCaptureBatch(ctx, batchDestination, sameSource, []sessioncapture.Delivery{next, deliveries[0]}); !errors.Is(err, ErrCheckpoint) || acks != nil {
		t.Fatal("out of order source chain accepted", acks, err)
	}
	if cp, err := s.Checkpoint(ctx, registrations[0].Key()); err != nil || cp != (sessioncapture.Checkpoint{}) {
		t.Fatal("failed source chain changed checkpoint", cp, err)
	}
	acks, err := s.AcceptCaptureBatch(ctx, batchDestination, sameSource, []sessioncapture.Delivery{deliveries[0], next})
	if err != nil || len(acks) != 2 {
		t.Fatal("ordered source chain rejected", acks, err)
	}
	// Suppression is checked even for a receipt replay. A different source's new
	// evidence earlier in the same group must roll back when replay is forbidden.
	if _, err := s.Redact(ctx, registrations[0].Namespace, "source", registrations[0].Namespace); err != nil {
		t.Fatal(err)
	}
	before := maps.Clone(s.healthChecks)
	if acks, err := s.AcceptCaptureBatch(ctx, batchDestination, []Registration{registrations[1], registrations[0]}, []sessioncapture.Delivery{deliveries[1], deliveries[0]}); err == nil || err.Error() != "capture_suppressed" || acks != nil {
		t.Fatal("suppressed replay accepted group", acks, err)
	}
	if !reflect.DeepEqual(before, s.healthChecks) {
		t.Fatal("suppressed group updated health cache")
	}
	if cp, err := s.Checkpoint(ctx, registrations[1].Key()); err != nil || cp != (sessioncapture.Checkpoint{}) {
		t.Fatal("suppressed group partially accepted unrelated source", cp, err)
	}
}

func TestCaptureBatchValidationAndCapacityHaveNoEffects(t *testing.T) {
	ctx := context.Background()
	s, registrations, deliveries, _ := batchFixture(t)
	for _, tc := range []struct {
		destination   string
		registrations []Registration
		deliveries    []sessioncapture.Delivery
	}{
		{"", registrations, deliveries},
		{batchDestination, registrations[:1], deliveries},
		{batchDestination, []Registration{registrations[1], registrations[0]}, deliveries},
	} {
		if acks, err := s.AcceptCaptureBatch(ctx, tc.destination, tc.registrations, tc.deliveries); err == nil || acks != nil {
			t.Fatal("invalid group accepted", acks, err)
		}
	}
	s.options.MinFreeBytes = ^uint64(0)
	if acks, err := s.AcceptCaptureBatch(ctx, batchDestination, registrations, deliveries); !errors.Is(err, ErrCapacity) || acks != nil {
		t.Fatal("capacity failure acknowledged", acks, err)
	}
	if len(s.healthChecks) != 0 {
		t.Fatal("capacity failure advanced health cache")
	}
	for _, r := range registrations {
		if cp, err := s.Checkpoint(ctx, r.Key()); err != nil || cp != (sessioncapture.Checkpoint{}) {
			t.Fatal("capacity failure checkpoint", cp, err)
		}
	}
	s.options.MinFreeBytes = 0
	acks, err := s.AcceptCaptureBatch(ctx, batchDestination, registrations, deliveries)
	if err != nil {
		t.Fatal(err)
	}
	// A durable replay needs no additional disk reservation or health update.
	before := maps.Clone(s.healthChecks)
	s.options.MinFreeBytes = ^uint64(0)
	replayed, err := s.AcceptCaptureBatch(ctx, batchDestination, registrations, deliveries)
	if err != nil || !reflect.DeepEqual(replayed, acks) || !reflect.DeepEqual(before, s.healthChecks) {
		t.Fatal("receipt replay depends on write headroom", replayed, err)
	}
}

func TestCaptureBatchLimitsRetainLargeScalarAcceptance(t *testing.T) {
	ctx := context.Background()
	s, registrations, deliveries, _ := batchFixture(t)
	manyRegistrations := make([]Registration, 17)
	manyDeliveries := make([]sessioncapture.Delivery, 17)
	for i := range manyDeliveries {
		manyRegistrations[i], manyDeliveries[i] = registrations[0], deliveries[0]
	}
	if acks, err := s.AcceptCaptureBatch(ctx, batchDestination, manyRegistrations, manyDeliveries); err == nil || acks != nil {
		t.Fatal("oversized delivery count accepted", acks, err)
	}
	largeRaw := []byte(fmt.Sprintf("{\"type\":\"user\",\"uuid\":\"large\",\"sessionId\":\"synthetic-claude\",\"message\":{\"role\":\"user\",\"content\":\"%s\"}}\n", strings.Repeat("x", 600000)))
	largeBatch := testBatch(t, largeRaw, registrations[0], sessioncapture.Checkpoint{})
	large, err := sessioncapture.NewDelivery(registrations[0].Source, sessioncapture.Checkpoint{}, largeBatch)
	if err != nil {
		t.Fatal(err)
	}
	if acks, err := s.AcceptCaptureBatch(ctx, batchDestination, registrations, []sessioncapture.Delivery{large, deliveries[1]}); err == nil || err.Error() != "capture_batch_too_large" || acks != nil {
		t.Fatal("oversized JSON group accepted", acks, err)
	}
	if cp, err := s.Checkpoint(ctx, registrations[0].Key()); err != nil || cp != (sessioncapture.Checkpoint{}) {
		t.Fatal("oversized group advanced checkpoint", cp, err)
	}
	sink, err := s.CaptureSink(batchDestination, registrations[0])
	if err != nil {
		t.Fatal(err)
	}
	if ack, err := sink.Accept(ctx, large); err != nil || ack.Validate(batchDestination, large) != nil {
		t.Fatal("scalar acceptance inherited small group limit", ack, err)
	}
}

func TestUnchangedCaptureMetadataDoesNotWrite(t *testing.T) {
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	for _, stmt := range []string{
		`CREATE TRIGGER fail_unchanged_source BEFORE UPDATE ON sources BEGIN SELECT RAISE(ABORT,'unchanged capabilities updated'); END`,
		`CREATE TRIGGER fail_unchanged_locator BEFORE UPDATE OF conversation_id ON stream_locators BEGIN SELECT RAISE(ABORT,'unchanged conversation updated'); END`,
		`CREATE TRIGGER fail_unchanged_checkpoint BEFORE UPDATE ON ingest_checkpoints BEGIN SELECT RAISE(ABORT,'unchanged checkpoint updated'); END`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	var before, after int64
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	empty := testBatch(t, raw, r, batch.Checkpoint)
	mustIngest(t, s, r, batch.Checkpoint, empty)
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&after); err != nil || after != before {
		t.Fatalf("unchanged capture mutated SQLite: %d -> %d: %v", before, after, err)
	}
}

func TestUnchangedHeadRetainsNewObservationAndChange(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	firstKey := batch.Records[0].Record.RecordKey
	if _, err := s.db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_unchanged_head BEFORE UPDATE ON record_heads WHEN OLD.record_key='%s' BEGIN SELECT RAISE(ABORT,'unchanged head updated'); END`, firstKey)); err != nil {
		t.Fatal(err)
	}
	rewritten := bytes.Replace(raw, []byte("I will check the archive boundaries."), []byte("I checked the archive boundaries."), 1)
	revised := testBatch(t, rewritten, r, batch.Checkpoint)
	mustIngest(t, s, r, batch.Checkpoint, revised)
	for _, table := range []string{"record_observations", "changes"} {
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE record_key=?", firstKey).Scan(&count); err != nil || count != 2 {
			t.Fatalf("%s lost attributable repeated observation: %d %v", table, count, err)
		}
	}
}

// WAL bytes measure SQLite page amplification independent of filesystem timing.
func BenchmarkCaptureBatchWAL(b *testing.B) {
	for _, grouped := range []bool{false, true} {
		name := "scalar"
		if grouped {
			name = "group16"
		}
		b.Run(name, func(b *testing.B) {
			opts := DefaultOptions()
			opts.MinFreeBytes = 0
			s, err := Open(filepath.Join(b.TempDir(), "archive"), opts)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()
			registrations := make([]Registration, 16)
			deliveries := make([]sessioncapture.Delivery, 16)
			for i := range registrations {
				registrations[i] = Registration{Source: sessioncapture.Source{Namespace: "fixture:wal", Provider: sessioncapture.Claude, StreamID: fmt.Sprintf("stream:%d", i)}, Root: "/synthetic", Path: fmt.Sprintf("stream-%d.jsonl", i)}
			}
			if err := s.Register(ctx, registrations); err != nil {
				b.Fatal(err)
			}
			if _, err := s.db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
				b.Fatal(err)
			}
			if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				b.Fatal(err)
			}
			previous := make([]sessioncapture.Checkpoint, 16)
			sources := make([][]byte, 16)
			b.ResetTimer()
			for b.Loop() {
				b.StopTimer()
				for i, r := range registrations {
					sources[i] = append(sources[i], []byte(fmt.Sprintf("{\"type\":\"user\",\"uuid\":\"record-%d-%d\",\"sessionId\":\"session-%d\",\"message\":{\"role\":\"user\",\"content\":\"Synthetic capture transaction\"}}\n", i, previous[i].Ordinal, i))...)
					batch, err := sessioncapture.Read(bytes.NewReader(sources[i]), r.Source, previous[i], sessioncapture.DefaultLimits(), testNow)
					if err != nil {
						b.Fatal(err)
					}
					deliveries[i], err = sessioncapture.NewDelivery(r.Source, previous[i], batch)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
				if grouped {
					if _, err := s.AcceptCaptureBatch(ctx, batchDestination, registrations, deliveries); err != nil {
						b.Fatal(err)
					}
				} else {
					for i, d := range deliveries {
						if _, err := s.AcceptCaptureBatch(ctx, batchDestination, registrations[i:i+1], []sessioncapture.Delivery{d}); err != nil {
							b.Fatal(err)
						}
					}
				}
				for i, d := range deliveries {
					previous[i] = d.Batch.Checkpoint
				}
			}
			b.StopTimer()
			info, err := os.Stat(filepath.Join(s.dir, "archive.sqlite-wal"))
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(info.Size())/float64(b.N), "wal-B/group")
			b.ReportMetric(float64(info.Size())/float64(b.N*len(registrations)), "wal-B/delivery")
		})
	}
}
