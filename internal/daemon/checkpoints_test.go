package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/algorhythmic/skald/capturespool"
	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/sessioncapture"
)

func attachCheckpointSpool(t *testing.T, c *Collector) {
	t.Helper()
	spool, err := capturespool.Open(filepath.Join(t.TempDir(), "spool"), []string{localDestination}, capturespool.Limits{MaxPending: 10, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spool.Close() })
	c.Spool = spool
}

func TestCheckpointSnapshotInvalidatesPendingDeliveryAndReplay(t *testing.T) {
	ctx := context.Background()
	c, _ := testCollector(t)
	attachCheckpointSpool(t, c)
	source := c.Config.Sources[0]
	c.loadCaptureCheckpoints(ctx)
	batch, err := captureFile(ctx, source, sessioncapture.Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	c.sources = []archive.Registration{}
	if err := c.acceptCapture(ctx, source, sessioncapture.Checkpoint{}, batch); err == nil || err.Error() != "capture_delivery_pending" {
		t.Fatalf("removed source delivery: %v", err)
	}
	if cp, err := c.captureCheckpoint(ctx, source); err != nil || cp != batch.Checkpoint {
		t.Fatalf("queued producer position was cached away: %+v %v", cp, err)
	}
	c.loadCaptureCheckpoints(ctx)
	if pending, err := c.capturePending(source.Key()); err != nil || pending != 1 {
		t.Fatalf("pending source absent: %d %v", pending, err)
	}
	c.sources = []archive.Registration{source}
	if n, err := c.Spool.Deliver(ctx, collectorSink{c}, 10); err != nil || n != 1 {
		t.Fatalf("retry: %d %v", n, err)
	}
	if !c.checkpoints.invalid[source.Key()] {
		t.Fatal("sink acceptance did not invalidate the cached position")
	}
	if pending, err := c.capturePending(source.Key()); err != nil || pending != 0 {
		t.Fatalf("acknowledgement left stale pending count: %d %v", pending, err)
	}
	if cp, err := c.captureCheckpoint(ctx, source); err != nil || cp != batch.Checkpoint {
		t.Fatalf("acknowledged producer position: %+v %v", cp, err)
	}
	// Replaying an immutable envelope uses its durable receipt without losing
	// the source's accepted position or manufacturing an extra pending batch.
	c.loadCaptureCheckpoints(ctx)
	if err := c.acceptCapture(ctx, source, sessioncapture.Checkpoint{}, batch); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if pending, err := c.capturePending(source.Key()); err != nil || pending != 0 {
		t.Fatalf("replay pending: %d %v", pending, err)
	}
}

func TestCheckpointSnapshotReconcilesReplacementAndClearsEveryPass(t *testing.T) {
	ctx := context.Background()
	c, _ := testCollector(t)
	attachCheckpointSpool(t, c)
	c.Scan(ctx)
	if c.checkpoints != nil {
		t.Fatal("snapshot survived its scan")
	}
	source := c.Config.Sources[0]
	c.loadCaptureCheckpoints(ctx)
	replacement, err := archive.Open(filepath.Join(t.TempDir(), "replacement"), c.Config.Options())
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := replacement.Register(ctx, c.Config.Sources); err != nil {
		t.Fatal(err)
	}
	c.Store = replacement
	if _, err := c.captureCheckpoint(ctx, source); err == nil || err.Error() != "capture_destination_checkpoint_mismatch" {
		t.Fatalf("cached prior archive accepted: %v", err)
	}
	c.Scan(ctx)
	if c.Issues()[source.Key()] != "capture_destination_checkpoint_mismatch" || c.checkpoints != nil {
		t.Fatalf("next pass hid replacement mismatch: %+v", c.Issues())
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	c.loadCaptureCheckpoints(ctx)
	if _, err := c.captureCheckpoint(canceled, source); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached checkpoint ignored cancellation: %v", err)
	}
	c.Scan(canceled)
	if c.checkpoints != nil {
		t.Fatal("canceled pass retained snapshot")
	}
}

func TestCheckpointSnapshotPartialTailAndSourceReuse(t *testing.T) {
	ctx := context.Background()
	c, file := testCollector(t)
	attachCheckpointSpool(t, c)
	c.Scan(ctx)
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(`{"type":"assistant","sessionId":"synthetic-claude","message":{"role":"assistant","content":"snapshot tail"}}`)
	if err := os.WriteFile(file, append(append([]byte{}, raw...), line...), 0600); err != nil {
		t.Fatal(err)
	}
	c.Scan(ctx)
	c.Scan(ctx)
	if err := os.WriteFile(file, append(append(append([]byte{}, raw...), line...), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	c.Scan(ctx)
	before, err := c.Store.Checkpoint(ctx, c.Config.Sources[0].Key())
	if err != nil || before.Offset != int64(len(raw)+len(line)+1) {
		t.Fatalf("completed tail lost: %+v %v", before, err)
	}
	replacement := []byte(`{"type":"user","sessionId":"reused-session","message":{"role":"user","content":"new source"}}` + "\n")
	if err := os.WriteFile(file, replacement, 0600); err != nil {
		t.Fatal(err)
	}
	c.Scan(ctx)
	after, err := c.Store.Checkpoint(ctx, c.Config.Sources[0].Key())
	if err != nil || after.Epoch <= before.Epoch || after.Offset != int64(len(replacement)) || len(c.Issues()) != 0 {
		t.Fatalf("source reuse resumed stale position: %+v %v %+v", after, err, c.Issues())
	}
}

func BenchmarkCaptureCheckpoints615Sources(b *testing.B) {
	ctx := context.Background()
	options := archive.DefaultOptions()
	options.MinFreeBytes = 0
	store, err := archive.Open(filepath.Join(b.TempDir(), "archive"), options)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	spool, err := capturespool.Open(filepath.Join(b.TempDir(), "spool"), []string{localDestination}, capturespool.Limits{MaxPending: 1024, MaxBytes: 16 << 20})
	if err != nil {
		b.Fatal(err)
	}
	defer spool.Close()
	sources := make([]archive.Registration, 615)
	root := b.TempDir()
	for i := range sources {
		sources[i] = archive.Registration{Source: sessioncapture.Source{Namespace: "fixture:checkpoints", Provider: sessioncapture.Claude, StreamID: fmt.Sprintf("source-%d", i)}, Root: root, Path: fmt.Sprintf("session-%d.jsonl", i)}
	}
	if err := store.Register(ctx, sources); err != nil {
		b.Fatal(err)
	}
	for i, source := range sources {
		raw := []byte(fmt.Sprintf(`{"type":"user","sessionId":"session-%d","message":{"role":"user","content":"fixture"}}`+"\n", i))
		batch, err := sessioncapture.Read(bytes.NewReader(raw), source.Source, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), time.Now())
		if err != nil {
			b.Fatal(err)
		}
		if err := store.Ingest(ctx, source, sessioncapture.Checkpoint{}, batch, time.Now()); err != nil {
			b.Fatal(err)
		}
		if err := spool.SeedCheckpoint(source.Source, batch.Checkpoint); err != nil {
			b.Fatal(err)
		}
	}
	c := &Collector{Store: store, Spool: spool}
	for _, snapshot := range []bool{false, true} {
		name := "point_reads"
		if snapshot {
			name = "bulk_snapshot"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if snapshot {
					c.loadCaptureCheckpoints(ctx)
				}
				for _, source := range sources {
					if _, err := c.captureCheckpoint(ctx, source); err != nil {
						b.Fatal(err)
					}
				}
				c.checkpoints = nil
			}
		})
	}
}
