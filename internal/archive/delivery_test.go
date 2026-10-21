package archive

import (
	"bytes"
	"context"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
)

func TestCaptureSinkAtomicAcceptanceAndStaleRetry(t *testing.T) {
	ctx := context.Background()
	s, registration, raw := testStore(t)
	batch := testBatch(t, raw, registration, sessioncapture.Checkpoint{})
	d, err := sessioncapture.NewDelivery(registration.Source, sessioncapture.Checkpoint{}, batch)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := s.CaptureSink("synthetic:archive", registration)
	if err != nil {
		t.Fatal(err)
	}
	// Fail after records/gaps would have been inserted, at the checkpoint write.
	if _, err := s.db.Exec(`CREATE TRIGGER interrupt_delivery BEFORE INSERT ON ingest_checkpoints BEGIN SELECT RAISE(ABORT,'synthetic interrupted acceptance'); END`); err != nil {
		t.Fatal(err)
	}
	if ack, err := sessioncapture.Deliver(ctx, sink, d); err == nil || ack != (sessioncapture.Acknowledgement{}) {
		t.Fatal("interrupted transaction acknowledged", err)
	}
	cp, err := s.Checkpoint(ctx, registration.Key())
	if err != nil || cp != (sessioncapture.Checkpoint{}) {
		t.Fatal("interrupted transaction advanced checkpoint", err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM artifact_versions").Scan(&count); err != nil || count != 0 {
		t.Fatal("interrupted transaction left partial records", err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM capture_receipts").Scan(&count); err != nil || count != 0 {
		t.Fatal("interrupted transaction left a receipt", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER interrupt_delivery"); err != nil {
		t.Fatal(err)
	}
	ack, err := sessioncapture.Deliver(ctx, sink, d)
	if err != nil || ack.Validate("synthetic:archive", d) != nil {
		t.Fatal("committed transaction not acknowledged", err)
	}
	// The receipt is committed with the candidate; an uncertain response can
	// retry the exact batch even though its previous checkpoint is now stale.
	if replayAck, err := sessioncapture.Deliver(ctx, sink, d); err != nil || replayAck != ack {
		t.Fatal("committed batch did not replay its exact receipt", err)
	}
	committed, err := s.Checkpoint(ctx, registration.Key())
	if err != nil || committed != d.Batch.Checkpoint {
		t.Fatal("explicit checkpoint reconciliation failed", err)
	}
	before, err := s.Stats(ctx, []string{registration.Namespace})
	if err != nil {
		t.Fatal(err)
	}
	replay := testBatch(t, raw, registration, committed)
	next, err := sessioncapture.NewDelivery(registration.Source, committed, replay)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessioncapture.Deliver(ctx, sink, next); err != nil {
		t.Fatal("reconciled read could not resume", err)
	}
	after, err := s.Stats(ctx, []string{registration.Namespace})
	if err != nil || after["record_versions"] != before["record_versions"] || after["boundary"] != before["boundary"] {
		t.Fatal("checkpoint reconciliation duplicated archive changes", err)
	}
}

func TestCaptureSinkLostResponseReopensAfterLaterCheckpoint(t *testing.T) {
	ctx := context.Background()
	s, registration, raw := testStore(t)
	firstLine := raw[:bytes.IndexByte(raw, '\n')+1]
	firstBatch := testBatch(t, firstLine, registration, sessioncapture.Checkpoint{})
	first, err := sessioncapture.NewDelivery(registration.Source, sessioncapture.Checkpoint{}, firstBatch)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := s.CaptureSink("synthetic:archive", registration)
	if err != nil {
		t.Fatal(err)
	}
	firstAck, err := sessioncapture.Deliver(ctx, sink, first)
	if err != nil {
		t.Fatal(err)
	}
	secondBatch := testBatch(t, raw, registration, firstBatch.Checkpoint)
	second, err := sessioncapture.NewDelivery(registration.Source, firstBatch.Checkpoint, secondBatch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessioncapture.Deliver(ctx, sink, second); err != nil {
		t.Fatal(err)
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
	reopenedSink, err := reopened.CaptureSink("synthetic:archive", registration)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := sessioncapture.Deliver(ctx, reopenedSink, first)
	if err != nil || replayed != firstAck {
		t.Fatalf("lost response retry after restart: %v %v", replayed, err)
	}
	cp, err := reopened.Checkpoint(ctx, registration.Key())
	if err != nil || cp != secondBatch.Checkpoint {
		t.Fatalf("retry regressed later checkpoint: %v %v", cp, err)
	}
	var count int
	if err := reopened.db.QueryRow("SELECT count(*) FROM capture_receipts").Scan(&count); err != nil || count != 2 {
		t.Fatalf("receipt ledger changed: %d %v", count, err)
	}
}

func TestCaptureSinkReceiptWriteFailureRollsBackAcceptance(t *testing.T) {
	ctx := context.Background()
	s, registration, raw := testStore(t)
	batch := testBatch(t, raw, registration, sessioncapture.Checkpoint{})
	d, err := sessioncapture.NewDelivery(registration.Source, sessioncapture.Checkpoint{}, batch)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := s.CaptureSink("synthetic:archive", registration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_capture_receipt BEFORE INSERT ON capture_receipts BEGIN SELECT RAISE(ABORT,'synthetic receipt write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if ack, err := sessioncapture.Deliver(ctx, sink, d); err == nil || ack != (sessioncapture.Acknowledgement{}) {
		t.Fatalf("failed receipt write acknowledged: %v %v", ack, err)
	}
	cp, err := s.Checkpoint(ctx, registration.Key())
	if err != nil || cp != (sessioncapture.Checkpoint{}) {
		t.Fatalf("failed receipt write advanced checkpoint: %v %v", cp, err)
	}
	for _, table := range []string{"artifact_versions", "capture_receipts"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s retained %d rows: %v", table, count, err)
		}
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_capture_receipt"); err != nil {
		t.Fatal(err)
	}
	if _, err := sessioncapture.Deliver(ctx, sink, d); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureSinkDestinationAndSourceIsolation(t *testing.T) {
	ctx := context.Background()
	left, registration, raw := testStore(t)
	right, other, _ := testStore(t)
	// Independent stores deliberately use the same logical source identity.
	other.Source = registration.Source
	if err := right.Register(ctx, []Registration{other}); err != nil {
		t.Fatal(err)
	}
	batch := testBatch(t, raw, registration, sessioncapture.Checkpoint{})
	d, err := sessioncapture.NewDelivery(registration.Source, sessioncapture.Checkpoint{}, batch)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := left.CaptureSink("synthetic:left", registration)
	b, _ := right.CaptureSink("synthetic:right", other)
	if ack, err := sessioncapture.Deliver(ctx, a, d); err != nil || ack.Destination != "synthetic:left" {
		t.Fatal(err)
	}
	cp, err := right.Checkpoint(ctx, registration.Key())
	if err != nil || cp != (sessioncapture.Checkpoint{}) {
		t.Fatal("one destination advanced another", err)
	}
	if ack, err := sessioncapture.Deliver(ctx, b, d); err != nil || ack.Destination != "synthetic:right" {
		t.Fatal(err)
	}
	wrong := registration
	wrong.StreamID = "another-stream"
	wrongSink, _ := left.CaptureSink("synthetic:left", wrong)
	if _, err := wrongSink.Accept(ctx, d); err == nil {
		t.Fatal("source binding accepted another stream")
	}
	if _, err := left.CaptureSink("", registration); err == nil {
		t.Fatal("unnamed destination accepted")
	}
}
