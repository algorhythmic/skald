package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/algorhythmic/skald/capturespool"
	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/sessioncapture"
)

const localDestination = "skald:standalone-archive"

// collectorSink authorizes queued deliveries against the current configuration,
// without reopening the native transcript. A missing file does not lose work.
type collectorSink struct{ collector *Collector }

func (collectorSink) Destination() string { return localDestination }
func (s collectorSink) Accept(ctx context.Context, d sessioncapture.Delivery) (sessioncapture.Acknowledgement, error) {
	for _, source := range s.collector.Sources() {
		if source.Source != d.Source {
			continue
		}
		sink, err := s.collector.Store.CaptureSink(localDestination, source)
		if err != nil {
			return sessioncapture.Acknowledgement{}, err
		}
		s.collector.invalidateCaptureCheckpoint(d.Source.Key())
		ack, err := sink.Accept(ctx, d)
		if err != nil {
			s.collector.issue(source.Key(), "capture_delivery_pending")
		}
		return ack, err
	}
	return sessioncapture.Acknowledgement{}, errors.New("delivery_source_not_registered")
}

// AcceptBatch resolves every configured source before the archive's atomic
// group transaction. No spool lock is held across this external sink call.
func (s collectorSink) AcceptBatch(ctx context.Context, deliveries []sessioncapture.Delivery) ([]sessioncapture.Acknowledgement, error) {
	sources := s.collector.Sources()
	registrations := make([]archive.Registration, len(deliveries))
	for i, d := range deliveries {
		found := false
		for _, source := range sources {
			if source.Source == d.Source {
				registrations[i] = source
				found = true
				s.collector.invalidateCaptureCheckpoint(d.Source.Key())
				break
			}
		}
		if !found {
			return nil, errors.New("delivery_source_not_registered")
		}
	}
	acks, err := s.collector.Store.AcceptCaptureBatch(ctx, localDestination, registrations, deliveries)
	if err != nil {
		s.collector.Metrics.Add("capture_group_fallbacks", 1)
	}
	return acks, err
}

// A snapshot lives for one Scan only. It is loaded after discovery and retry
// delivery, and each source is invalidated before a producer or sink mutation.
// Every new pass reconciles durable positions, including external repairs and
// archive replacement; no time-based cache can hide a destination mismatch.
type captureCheckpointSnapshot struct {
	store      *archive.Store
	spool      *capturespool.Spool
	archive    map[string]archive.CheckpointState
	queued     map[string]capturespool.CheckpointState
	archiveErr error
	spoolErr   error
	invalid    map[string]bool
}

func (c *Collector) loadCaptureCheckpoints(ctx context.Context) {
	started := time.Now()
	defer func() { c.Metrics.Duration("checkpoint_snapshot", time.Since(started)) }()
	c.Metrics.Add("checkpoint_snapshot_loads", 1)
	s := &captureCheckpointSnapshot{store: c.Store, spool: c.Spool, invalid: map[string]bool{}}
	s.archive, s.archiveErr = c.Store.CheckpointSnapshot(ctx)
	if c.Spool != nil {
		s.queued, s.spoolErr = c.Spool.CheckpointSnapshot(ctx, localDestination)
	}
	if s.archiveErr != nil || s.spoolErr != nil {
		c.Metrics.Add("checkpoint_snapshot_errors", 1)
	}
	c.checkpoints = s
}

func (c *Collector) invalidateCaptureCheckpoint(key string) {
	if c.checkpoints != nil {
		c.checkpoints.invalid[key] = true
	}
}

func (c *Collector) capturePending(key string) (int, error) {
	if s := c.checkpoints; s != nil && s.store == c.Store && s.spool == c.Spool && !s.invalid[key] {
		if s.spoolErr != nil {
			return 0, s.spoolErr
		}
		queued := s.queued[key]
		return queued.Pending, queued.Err
	}
	status, err := c.Spool.StatusForSource(key)
	return status.Pending, err
}

func (c *Collector) captureCheckpoint(ctx context.Context, source archive.Registration) (sessioncapture.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return sessioncapture.Checkpoint{}, err
	}
	if s := c.checkpoints; s != nil && s.store == c.Store && s.spool == c.Spool && !s.invalid[source.Key()] {
		if s.spoolErr != nil {
			return sessioncapture.Checkpoint{}, s.spoolErr
		}
		if queued, found := s.queued[source.Key()]; found {
			if queued.Err != nil {
				return queued.Checkpoint, queued.Err
			}
			if queued.Acknowledged {
				stored := s.archive[source.Key()]
				if s.archiveErr != nil {
					return queued.Checkpoint, s.archiveErr
				}
				if stored.Err != nil {
					return queued.Checkpoint, stored.Err
				}
				if checkpointMismatch(stored.Checkpoint, queued.Accepted, queued.Pending) {
					return queued.Checkpoint, errors.New("capture_destination_checkpoint_mismatch")
				}
			}
			return queued.Checkpoint, nil
		}
		if s.archiveErr != nil {
			return sessioncapture.Checkpoint{}, s.archiveErr
		}
		stored := s.archive[source.Key()]
		if stored.Err != nil {
			return stored.Checkpoint, stored.Err
		}
		return c.seedCaptureCheckpoint(source, stored.Checkpoint)
	}
	if c.Spool != nil {
		cp, found, err := c.Spool.Checkpoint(source.Key())
		if err != nil {
			return cp, err
		}
		if found {
			accepted, acknowledged, err := c.Spool.DestinationCheckpoint(source.Key(), localDestination)
			if err != nil {
				return cp, err
			}
			if acknowledged {
				stored, err := c.Store.Checkpoint(ctx, source.Key())
				if err != nil {
					return cp, err
				}
				status, err := c.Spool.StatusForSource(source.Key())
				if err != nil {
					return cp, err
				}
				if checkpointMismatch(stored, accepted, status.Pending) {
					return cp, errors.New("capture_destination_checkpoint_mismatch")
				}
			}
			return cp, nil
		}
	}
	cp, err := c.Store.Checkpoint(ctx, source.Key())
	if err != nil {
		return cp, err
	}
	return c.seedCaptureCheckpoint(source, cp)
}

func checkpointMismatch(stored, accepted sessioncapture.Checkpoint, pending int) bool {
	return stored != accepted && (pending == 0 || stored.Epoch < accepted.Epoch || (stored.Epoch == accepted.Epoch && stored.Offset < accepted.Offset))
}

func (c *Collector) seedCaptureCheckpoint(source archive.Registration, cp sessioncapture.Checkpoint) (sessioncapture.Checkpoint, error) {
	if c.Spool != nil && cp.Version != 0 {
		c.invalidateCaptureCheckpoint(source.Key())
		return cp, c.Spool.SeedCheckpoint(source.Source, cp)
	}
	return cp, nil
}

// DeliveryStatus reports only envelopes belonging to the requested configured
// namespaces. Another namespace's backlog must not leak through an aggregate.
func (c *Collector) DeliveryStatus(namespaces []string) (map[string]any, error) {
	if c.Spool == nil {
		return map[string]any{"enabled": false}, nil
	}
	limits, err := c.Spool.Status()
	if err != nil {
		return nil, err
	}
	total := capturespool.Status{ByDestination: map[string]int{localDestination: 0}, MaxPending: limits.MaxPending, MaxBytes: limits.MaxBytes}
	for _, namespace := range namespaces {
		status, err := c.Spool.StatusForNamespace(namespace)
		if err != nil {
			return nil, err
		}
		total.Pending += status.Pending
		total.PendingBytes += status.PendingBytes
		total.MaxPending, total.MaxBytes = status.MaxPending, status.MaxBytes
		for destination, pending := range status.ByDestination {
			total.ByDestination[destination] += pending
		}
	}
	return map[string]any{"enabled": true, "pending": total.Pending, "pending_bytes": total.PendingBytes,
		"by_destination": total.ByDestination, "max_pending": total.MaxPending, "max_bytes": total.MaxBytes}, nil
}

func (c *Collector) requireDrained(ctx context.Context) error {
	if c.Spool == nil {
		return nil
	}
	_, _ = c.Spool.Deliver(ctx, collectorSink{c}, 1024)
	status, err := c.Spool.Status()
	if err != nil {
		return err
	}
	if status.Pending != 0 {
		return errors.New("capture_spool_pending")
	}
	return nil
}

// Backup exports an archive only after delivery catches up. The live spool is
// not copied into the archive backup or silently discarded during restore.
func (c *Collector) Backup(ctx context.Context) (string, error) {
	c.scanMu.Lock()
	defer c.scanMu.Unlock()
	if err := c.requireDrained(ctx); err != nil {
		return "", err
	}
	return c.Store.Backup(ctx)
}

// Redact holds the collector lock through drainage and removal so no captured
// source bytes remain queued while a successful removal receipt is returned.
func (c *Collector) Redact(ctx context.Context, namespace, kind, target string) (archive.RedactionReceipt, error) {
	c.scanMu.Lock()
	defer c.scanMu.Unlock()
	if err := c.requireDrained(ctx); err != nil {
		return archive.RedactionReceipt{}, err
	}
	receipt, err := c.Store.Redact(ctx, namespace, kind, target)
	if err == nil {
		c.verified = nil
		c.checkpoints = nil
	}
	return receipt, err
}
