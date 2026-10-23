package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/algorhythmic/skald/capturespool"
	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/sessioncapture"
)

// Staging is local to the scanMu critical section. The 25ms age target is
// cooperative between native captures; one bounded capture may take longer.
const captureStageAge = 25 * time.Millisecond

type captureCandidate struct {
	source   archive.Registration
	previous sessioncapture.Checkpoint
	batch    sessioncapture.Batch
	delivery sessioncapture.Delivery
	stamp    fileStamp
	stampErr error
}
type captureStage struct {
	collector  *Collector
	candidates []captureCandidate
	bytes      int
	started    time.Time
}

func (s *captureStage) flush(ctx context.Context) bool {
	if len(s.candidates) == 0 {
		return false
	}
	candidates := s.candidates
	s.candidates = nil
	s.bytes = 0
	s.started = time.Time{}
	c := s.collector
	valid := []captureCandidate{}
	deliveries := []sessioncapture.Delivery{}
	for _, candidate := range candidates {
		if err := c.Store.CheckCapture(ctx, candidate.source, candidate.batch); err != nil {
			c.finishCapture(ctx, candidate, err)
			continue
		}
		valid = append(valid, candidate)
		deliveries = append(deliveries, candidate.delivery)
		c.invalidateCaptureCheckpoint(candidate.source.Key())
	}
	if len(valid) == 0 {
		return false
	}
	c.Metrics.Add("capture_group_flushes", 1)
	c.Metrics.Add("capture_group_batches", int64(len(valid)))
	enqueueErr := c.Spool.EnqueueBatch(ctx, deliveries, []string{localDestination})
	if enqueueErr != nil && ctx.Err() == nil {
		c.Metrics.Add("capture_group_fallbacks", 1)
	}
	enqueued := make([]bool, len(valid))
	for i, candidate := range valid {
		err := enqueueErr
		if err != nil && ctx.Err() == nil {
			err = c.Spool.Enqueue(candidate.delivery, []string{localDestination})
		}
		if err != nil {
			c.finishCapture(ctx, candidate, err)
		} else {
			enqueued[i] = true
		}
	}
	// The producer positions above are durable, but source observations become
	// successful only after destination acknowledgement and spool receipt commit.
	_, _ = c.Spool.Deliver(ctx, collectorSink{c}, 256)
	more := false
	for i, candidate := range valid {
		if !enqueued[i] {
			continue
		}
		accepted, err := c.Spool.Acknowledged(candidate.delivery.BatchID, localDestination)
		if err == nil && !accepted {
			err = errors.New("capture_delivery_pending")
		}
		if c.finishCapture(ctx, candidate, err) {
			more = more || candidate.batch.More
		}
	}
	return more
}
func (s *captureStage) add(ctx context.Context, candidate captureCandidate) (bool, bool) {
	if s.collector.Spool == nil || (len(candidate.batch.Records) == 0 && len(candidate.batch.Gaps) == 0 && candidate.previous.Version != 0) {
		return false, false
	}
	delivery, err := sessioncapture.NewDelivery(candidate.source.Source, candidate.previous, candidate.batch)
	if err != nil {
		return false, false
	}
	wire, err := json.Marshal(delivery)
	if err != nil || len(wire) > capturespool.MaxBatchBytes {
		return false, false
	}
	more := false
	if len(s.candidates) > 0 && (len(s.candidates) >= capturespool.MaxBatchDeliveries || s.bytes+len(wire) > capturespool.MaxBatchBytes || time.Since(s.started) >= captureStageAge) {
		more = s.flush(ctx)
	}
	candidate.delivery = delivery
	if len(s.candidates) == 0 {
		s.started = time.Now()
	}
	s.candidates = append(s.candidates, candidate)
	s.bytes += len(wire)
	if len(s.candidates) >= capturespool.MaxBatchDeliveries || s.bytes >= capturespool.MaxBatchBytes {
		more = s.flush(ctx) || more
	}
	return true, more
}

// finishCapture preserves the same issue/health/cache rules for staged and
// immediate deliveries. The original source stamp must still match at commit.
func (c *Collector) finishCapture(ctx context.Context, candidate captureCandidate, err error) bool {
	source, batch := candidate.source, candidate.batch
	if err != nil {
		delete(c.acceptedPolls, source.Key())
		code := "archive_write_failed"
		if errors.Is(err, archive.ErrCapacity) || errors.Is(err, capturespool.ErrCapacity) {
			code = "capture_capacity_limit"
		}
		if err.Error() == "capture_suppressed" {
			code = "capture_suppressed"
		}
		if err.Error() == "capture_delivery_pending" {
			code = "capture_delivery_pending"
		}
		c.issue(source.Key(), code)
		_ = c.Store.ObserveHealth(ctx, source, "blocked", time.Now())
		return false
	}
	if c.acceptedPolls == nil {
		c.acceptedPolls = map[string]capturePoll{}
	}
	c.acceptedPolls[source.Key()] = capturePollFor(batch)
	c.Metrics.Add("captured_records", int64(len(batch.Records)))
	health, issue := captureHealth(batch)
	if err := c.Store.ObserveHealth(ctx, source, health, time.Now()); err != nil {
		c.issue(source.Key(), "archive_write_failed")
		return false
	}
	c.issue(source.Key(), issue)
	consumed := batch.Checkpoint.Offset == candidate.stamp.Size
	if source.Provider == sessioncapture.Devin {
		consumed = true
	}
	cacheable := !batch.More && (consumed || batch.Pending || batch.Blocked)
	if source.Provider == sessioncapture.Devin && batch.Pending {
		cacheable = false
	}
	if cacheable && candidate.stampErr == nil {
		if after, err := sourceStamp(source); err == nil && after == candidate.stamp {
			if c.verified == nil {
				c.verified = map[string]verifiedFile{}
			}
			c.verified[source.Key()] = verifiedFile{Stamp: candidate.stamp, Checkpoint: batch.Checkpoint, At: time.Now(), Health: health, Issue: issue}
		}
	}
	return true
}
