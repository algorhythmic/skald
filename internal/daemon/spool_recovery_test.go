package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/algorhythmic/skald/capturespool"
	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/sessioncapture"
)

// A receipt for an earlier envelope can have the same candidate checkpoint as
// a later metadata-only envelope. The later envelope still needs its own ack.
func TestMetadataOnlyEnvelopeNeedsItsOwnReceipt(t *testing.T) {
	ctx := context.Background()
	c, _ := testCollector(t)
	spool, err := capturespool.Open(filepath.Join(t.TempDir(), "spool"), []string{localDestination}, capturespool.Limits{MaxPending: 10, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	c.Spool = spool
	c.Scan(ctx)
	source := c.Config.Sources[0]
	cp, ok, err := spool.DestinationCheckpoint(source.Key(), localDestination)
	if err != nil || !ok {
		t.Fatalf("initial ack: %v %v", ok, err)
	}
	// Simulate a source removed from the configured list between enqueue and
	// delivery. The archive registration still exists, so enqueue can succeed.
	c.sources = []archive.Registration{}
	batch := sessioncapture.Batch{Records: []sessioncapture.Captured{}, Gaps: []sessioncapture.Gap{}, Checkpoint: cp, More: true}
	if err := c.acceptCapture(ctx, source, cp, batch); err == nil || err.Error() != "capture_delivery_pending" {
		t.Fatalf("metadata-only envelope falsely accepted: %v", err)
	}
	status, err := spool.Status()
	if err != nil || status.Pending != 1 {
		t.Fatalf("pending envelope missing: %+v %v", status, err)
	}
}

func TestArchiveReplacementCannotInheritAheadSpoolCheckpoint(t *testing.T) {
	ctx := context.Background()
	c, _ := testCollector(t)
	spool, err := capturespool.Open(filepath.Join(t.TempDir(), "spool"), []string{localDestination}, capturespool.Limits{MaxPending: 10, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	c.Spool = spool
	c.Scan(ctx)
	source := c.Config.Sources[0]
	spoolCP, found, err := spool.Checkpoint(source.Key())
	if err != nil || !found || spoolCP.Version != 1 {
		t.Fatalf("capture progress absent: %v %v %v", spoolCP, found, err)
	}
	status, err := spool.Status()
	if err != nil || status.Pending != 0 {
		t.Fatalf("spool should be drained: %+v %v", status, err)
	}
	// An older restore or wrong archive path has no accepted checkpoint, while
	// this spool still claims a later producer position.
	replacement, err := archive.Open(filepath.Join(t.TempDir(), "replacement"), c.Config.Options())
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := replacement.Register(ctx, c.Config.Sources); err != nil {
		t.Fatal(err)
	}
	c.Store = replacement
	if cp, err := c.captureCheckpoint(ctx, source); err == nil {
		t.Fatalf("archive behind spool accepted as current: %v", cp)
	}
}

func TestRemovedSourceBacklogVisibleAndBlocksBackupRedaction(t *testing.T) {
	ctx := context.Background()
	c, _ := testCollector(t)
	spool, err := capturespool.Open(filepath.Join(t.TempDir(), "spool"), []string{localDestination}, capturespool.Limits{MaxPending: 10, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	c.Spool = spool
	source := c.Config.Sources[0]
	batch, err := captureFile(ctx, source, sessioncapture.Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	// The current source registry drops the stream while its immutable batch
	// remains queued. Its namespace is still in the configured reader scope.
	c.sources = []archive.Registration{}
	if err := c.acceptCapture(ctx, source, sessioncapture.Checkpoint{}, batch); err == nil || err.Error() != "capture_delivery_pending" {
		t.Fatalf("queued removed source accepted: %v", err)
	}
	view, err := c.DeliveryStatus([]string{source.Namespace})
	if err != nil || view["pending"] != 1 {
		t.Fatalf("removed source backlog hidden: %+v %v", view, err)
	}
	if _, err := c.Backup(ctx); err == nil || err.Error() != "capture_spool_pending" {
		t.Fatalf("backup ignored pending source: %v", err)
	}
	if _, err := c.Redact(ctx, source.Namespace, "source", source.Namespace); err == nil || err.Error() != "capture_spool_pending" {
		t.Fatalf("redaction ignored pending source: %v", err)
	}
}

func TestInitializeRejectsDifferentArchiveInstance(t *testing.T) {
	ctx := context.Background()
	c, _ := testCollector(t)
	spool, err := capturespool.Open(filepath.Join(t.TempDir(), "spool"), []string{localDestination}, capturespool.Limits{MaxPending: 10, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	c.Spool = spool
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	replacement, err := archive.Open(filepath.Join(t.TempDir(), "replacement"), c.Config.Options())
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	c.Store = replacement
	if err := c.Initialize(ctx); err == nil || err != capturespool.ErrBinding {
		t.Fatalf("new archive inherited old spool binding: %v", err)
	}
}
