package archive

import (
	"context"
	"errors"
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
	if _, err := s.db.Exec("DROP TRIGGER interrupt_delivery"); err != nil {
		t.Fatal(err)
	}
	ack, err := sessioncapture.Deliver(ctx, sink, d)
	if err != nil || ack.Validate("synthetic:archive", d) != nil {
		t.Fatal("committed transaction not acknowledged", err)
	}
	// A lost response may cause this same retry. No durable receipt ledger exists,
	// so a matching candidate checkpoint alone must never manufacture a receipt.
	if ack, err := sessioncapture.Deliver(ctx, sink, d); !errors.Is(err, ErrCheckpoint) || ack != (sessioncapture.Acknowledgement{}) {
		t.Fatal("stale prior checkpoint manufactured a receipt", err)
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
