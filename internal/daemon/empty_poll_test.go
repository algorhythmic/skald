package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/algorhythmic/skald/capturespool"
	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/internal/observability"
	"github.com/algorhythmic/skald/sessioncapture"
)

func collectorWithCountedSpool(t *testing.T) (*Collector, func() int) {
	t.Helper()
	c, _ := testCollector(t)
	c.Metrics = observability.New()
	dir := filepath.Join(t.TempDir(), "spool")
	spool, err := capturespool.Open(dir, []string{localDestination}, capturespool.Limits{MaxPending: 32, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spool.Close() })
	c.Spool = spool
	db, err := sql.Open("sqlite", archive.DSN(filepath.Join(dir, "capture-spool.sqlite"), true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return c, func() int {
		t.Helper()
		var count int
		if err := db.QueryRow("SELECT count(*) FROM batch_ids").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
}

func TestRedundantEmptyPollsRetainMetadataAndActivityEdges(t *testing.T) {
	c, count := collectorWithCountedSpool(t)
	ctx := context.Background()
	c.Scan(ctx)
	source := c.Config.Sources[0]
	cp, err := c.Store.Checkpoint(ctx, source.Key())
	if err != nil {
		t.Fatal(err)
	}
	batch := sessioncapture.Batch{Records: []sessioncapture.Captured{}, Gaps: []sessioncapture.Gap{}, Checkpoint: cp}
	accept := func(delta int) {
		t.Helper()
		before := count()
		if err := c.acceptCapture(ctx, source, cp, batch); err != nil {
			t.Fatal(err)
		}
		if got := count(); got != before+delta {
			t.Fatalf("delivery count: %d -> %d, wanted delta %d (%+v)", before, got, delta, capturePollFor(batch))
		}
	}
	// An idle audit following record capture adds no evidence.
	accept(0)
	batch.Pending = true
	batch.SourceActiveAt = time.Now()
	accept(1)
	for i := 0; i < 10; i++ {
		batch.SourceActiveAt = batch.SourceActiveAt.Add(time.Second)
		accept(0) // New inactive bookkeeping timestamps carry no liveness.
	}
	batch.SourceActive = true
	accept(1)
	accept(0)
	batch.SourceActiveAt = batch.SourceActiveAt.Add(time.Second)
	accept(1) // Active timestamps refresh durable liveness.
	batch.SourceActive = false
	accept(1)
	batch.Pending = false
	accept(1)
	batch.Blocked = true
	accept(1)
	batch.More = true
	accept(1)
	batch.Gaps = []sessioncapture.Gap{{Code: "synthetic_terminal_gap", Offset: cp.Offset, Ordinal: cp.Ordinal, Generation: cp.Generation}}
	accept(1)
	skipped := c.Metrics.Snapshot().Counters["empty_polls_skipped"]
	accept(0) // The spool deduplicates the exact gap delivery itself.
	if got := c.Metrics.Snapshot().Counters["empty_polls_skipped"]; got != skipped {
		t.Fatal("gap-carrying envelope was suppressed", skipped, got)
	}
}

func TestCanceledScanFlushesCompletedHealthChecks(t *testing.T) {
	c, _ := testCollector(t)
	c.Metrics = observability.New()
	source := c.Config.Sources[0]
	now := time.Now()
	if err := c.Store.SetHealth(context.Background(), source, "available", now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.ObserveHealth(context.Background(), source, "available", now); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Scan(ctx)
	counters := c.Metrics.Snapshot().Counters
	if counters["health_heartbeat_transactions"] != 1 || counters["health_heartbeat_sources"] != 1 || counters["health_flush_errors"] != 0 {
		t.Fatal("cancellation lost a completed observation", counters)
	}
	if n, err := c.Store.FlushHealthBatch(context.Background()); err != nil || n != 0 {
		t.Fatal("canceled scan left a due heartbeat uncommitted", n, err)
	}
}

func TestRedundantEmptyPollDoesNotBypassPendingDeliveryOrRedaction(t *testing.T) {
	c, count := collectorWithCountedSpool(t)
	ctx := context.Background()
	c.Scan(ctx)
	source := c.Config.Sources[0]
	cp, err := c.Store.Checkpoint(ctx, source.Key())
	if err != nil {
		t.Fatal(err)
	}
	empty := sessioncapture.Batch{Records: []sessioncapture.Captured{}, Gaps: []sessioncapture.Gap{}, Checkpoint: cp}
	pending := empty
	pending.More = true
	delivery, err := sessioncapture.NewDelivery(source.Source, cp, pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Spool.Enqueue(delivery, []string{localDestination}); err != nil {
		t.Fatal(err)
	}
	c.sources = []archive.Registration{}
	before := count()
	if err := c.acceptCapture(ctx, source, cp, empty); err == nil || err.Error() != "capture_delivery_pending" {
		t.Fatal("redundant poll concealed uncertain metadata envelope", err)
	}
	if got := count(); got != before+1 {
		t.Fatal("empty poll incorrectly skipped pending delivery", before, got)
	}
	c.sources = nil
	c.Scan(ctx)
	if status, err := c.Spool.Status(); err != nil || status.Pending != 0 {
		t.Fatal("retry did not drain metadata envelopes", status, err)
	}
	// Establish the same successfully accepted poll state, then redact it.
	if err := c.acceptCapture(ctx, source, cp, empty); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Redact(ctx, source.Namespace, "source", source.Namespace); err != nil {
		t.Fatal(err)
	}
	before = count()
	if err := c.acceptCapture(ctx, source, cp, empty); err == nil || err.Error() != "capture_suppressed" {
		t.Fatal("suppressed empty poll bypassed source tombstone", err)
	}
	if got := count(); got != before {
		t.Fatal("suppressed metadata entered spool", before, got)
	}
}

func TestFirstEmptyCheckpointIsDelivered(t *testing.T) {
	c, count := collectorWithCountedSpool(t)
	ctx := context.Background()
	source := c.Config.Sources[0]
	batch, err := sessioncapture.Read(bytes.NewReader(nil), source.Source, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.acceptCapture(ctx, source, sessioncapture.Checkpoint{}, batch); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatal("initial empty checkpoint was discarded")
	}
	if cp, err := c.Store.Checkpoint(ctx, source.Key()); err != nil || cp != batch.Checkpoint {
		t.Fatal("initial empty checkpoint was not durable", cp, err)
	}
	if err := c.acceptCapture(ctx, source, batch.Checkpoint, batch); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatal("unchanged initialized checkpoint was enqueued again")
	}
}
