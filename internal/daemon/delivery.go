package daemon

import (
	"context"
	"errors"

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
		ack, err := sink.Accept(ctx, d)
		if err != nil {
			s.collector.issue(source.Key(), "capture_delivery_pending")
		}
		return ack, err
	}
	return sessioncapture.Acknowledgement{}, errors.New("delivery_source_not_registered")
}

func (c *Collector) captureCheckpoint(ctx context.Context, source archive.Registration) (sessioncapture.Checkpoint, error) {
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
				if stored != accepted && (status.Pending == 0 || stored.Epoch < accepted.Epoch || (stored.Epoch == accepted.Epoch && stored.Offset < accepted.Offset)) {
					return cp, errors.New("capture_destination_checkpoint_mismatch")
				}
			}
			return cp, nil
		}
	}
	cp, err := c.Store.Checkpoint(ctx, source.Key())
	if err == nil && c.Spool != nil && cp.Version != 0 {
		err = c.Spool.SeedCheckpoint(source.Source, cp)
	}
	return cp, err
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
	}
	return receipt, err
}
