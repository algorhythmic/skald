package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/algorhythmic/skald/capturespool"
	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/internal/observability"
	"github.com/algorhythmic/skald/sessioncapture"
)

type Collector struct {
	Metrics         *observability.Recorder
	scanMu          sync.Mutex
	sources         []archive.Registration
	discovery       map[string]DiscoveryStatus
	lastDiscovery   time.Time
	discoveryCursor map[string]int
	verified        map[string]verifiedFile
	checkpoints     *captureCheckpointSnapshot
	devinWal        map[string]walTick
	acceptedPolls   map[string]capturePoll
	Store           *archive.Store
	Spool           *capturespool.Spool
	Config          Config
	mu              sync.Mutex
	issues          map[string]string
	heartbeatIssues map[string]string
}

func (c *Collector) Issues() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for k, v := range c.issues {
		out[k] = v
	}
	for key, code := range c.heartbeatIssues {
		if out[key] == "" {
			out[key] = code
		}
	}
	return out
}

func (c *Collector) heartbeatIssue(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.heartbeatIssues = nil
		return
	}
	var heartbeatErr *archive.HealthHeartbeatError
	if errors.As(err, &heartbeatErr) {
		c.heartbeatIssues = make(map[string]string, len(heartbeatErr.Keys))
		for _, key := range heartbeatErr.Keys {
			c.heartbeatIssues[key] = "archive_write_failed"
		}
	}
}
func (c *Collector) issue(key, code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.issues == nil {
		c.issues = map[string]string{}
	}
	if code == "" {
		delete(c.issues, key)
	} else {
		c.issues[key] = code
	}
}

// Scan takes one bounded batch from every source for fairness during backfill.
// File identity, size, mtime and ctime are checked on every pass. Complete stable
// files use a bounded verification cache; changes and periodic audits reread
// prefixes. Filesystem notifications are never the source of truth.
func (c *Collector) Scan(ctx context.Context) bool {
	c.scanMu.Lock()
	defer c.scanMu.Unlock()
	started := time.Now()
	c.Metrics.Add("scans", 1)
	defer func() { c.Metrics.Duration("scan", time.Since(started)) }()
	defer func() {
		// Already completed observations remain valid if the scan is canceled.
		// Give their grouped durable heartbeat a bounded chance to commit.
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		c.Metrics.Add("health_flush_calls", 1)
		if n, err := c.Store.FlushHealthBatch(flushCtx); err != nil {
			c.Metrics.Add("health_flush_errors", 1)
			c.heartbeatIssue(err)
		} else {
			if n > 0 {
				c.Metrics.Add("health_heartbeat_transactions", 1)
				c.Metrics.Add("health_heartbeat_sources", int64(n))
			}
			c.heartbeatIssue(nil)
		}
	}()
	c.discover(ctx)
	// Retry already captured envelopes even when the native source is offline.
	if c.Spool != nil {
		_, _ = c.Spool.Deliver(ctx, collectorSink{c}, 256)
	}
	c.loadCaptureCheckpoints(ctx)
	defer func() { c.checkpoints = nil }()
	more := false
	stage := captureStage{collector: c}
	defer func() { stage.flush(ctx) }()
	for _, source := range c.Sources() {
		if len(stage.candidates) > 0 && time.Since(stage.started) >= captureStageAge {
			more = stage.flush(ctx) || more
		}
		if ctx.Err() != nil {
			return false
		}
		c.Metrics.Add("source_checks", 1)
		cp, err := c.captureCheckpoint(ctx, source)
		if err != nil {
			code := "checkpoint_read_failed"
			if err.Error() == "capture_destination_checkpoint_mismatch" {
				code = "capture_destination_checkpoint_mismatch"
			}
			c.issue(source.Key(), code)
			continue
		}

		stamp, stampErr := sourceStamp(source)
		if cached, ok := c.verified[source.Key()]; ok && stampErr == nil && cached.Stamp == stamp && cached.Checkpoint == cp && time.Since(cached.At) < time.Minute {
			c.Metrics.Add("file_cache_hits", 1)
			if err := c.Store.ObserveHealth(ctx, source, cached.Health, time.Now()); err != nil {
				c.issue(source.Key(), "archive_write_failed")
			} else {
				c.issue(source.Key(), cached.Issue)
			}
			continue
		}
		delete(c.verified, source.Key())
		var batch sessioncapture.Batch
		if source.Provider == sessioncapture.Devin {
			if walAt, hot := sessioncapture.DevinWal(source.Root, source.Path); hot {
				// The desktop app writes bookkeeping pages to every open
				// session's WAL; only growing message content is activity.
				if c.devinWal == nil {
					c.devinWal = map[string]walTick{}
				}
				t := c.devinWal[source.Key()]
				t.at = walAt
				if count, maxPos, perr := sessioncapture.DevinActivity(ctx, source.Root, source.Path); perr == nil {
					t.active = count != t.count || maxPos != t.maxPos
					t.count, t.maxPos = count, maxPos
				}
				c.devinWal[source.Key()] = t
				candidate := cp
				if candidate.Version == 0 {
					initial, e := sessioncapture.Read(bytes.NewReader(nil), source.Source, cp, sessioncapture.DefaultLimits(), time.Now())
					if e != nil {
						c.issue(source.Key(), "checkpoint_read_failed")
						continue
					}
					candidate = initial.Checkpoint
				}
				batch = sessioncapture.Batch{Records: []sessioncapture.Captured{}, Gaps: []sessioncapture.Gap{}, Checkpoint: candidate, Pending: true, SourceActive: t.active, SourceActiveAt: walAt}
			} else if t, ok := c.devinWal[source.Key()]; ok && t.active {
				// Quiet again: a later bookkeeping touch must not read as a
				// continuation of an earlier live streak.
				c.devinWal[source.Key()] = walTick{count: t.count, maxPos: t.maxPos}
			}
		}
		if !batch.Pending {
			c.Metrics.Add("capture_attempts", 1)
			captureStarted := time.Now()
			batch, err = captureFileMeasured(ctx, source, cp, c.Metrics)
			c.Metrics.Duration("capture", time.Since(captureStarted))
		}
		if err != nil {
			c.Metrics.Add("capture_errors", 1)
			code := "source_unavailable"
			if errors.Is(err, context.Canceled) {
				return false
			}
			if err.Error() == "source_changed_during_read" || err.Error() == "source_replaced_during_read" {
				code = "source_changed_during_read"
			}
			c.issue(source.Key(), code)
			_ = c.Store.ObserveHealth(ctx, source, "unavailable", time.Now())
			continue
		}
		candidate := captureCandidate{source: source, previous: cp, batch: batch, stamp: stamp, stampErr: stampErr}
		if staged, flushedMore := stage.add(ctx, candidate); staged {
			more = more || flushedMore
			continue
		}
		more = stage.flush(ctx) || more
		if c.finishCapture(ctx, candidate, c.acceptCapture(ctx, source, cp, batch)) {
			more = more || batch.More
		}
	}
	more = stage.flush(ctx) || more
	return more
}

func captureHealth(batch sessioncapture.Batch) (health, issue string) {
	health = "available"
	if batch.Pending && !batch.SourceActive {
		health = "partial_line"
	}
	if batch.More {
		health = "backfilling"
	}
	if batch.Checkpoint.HasGaps {
		health = "capture_gaps"
	}
	if batch.Blocked {
		health, issue = "blocked", "capture_blocked"
	}
	return
}

// A producer checkpoint advances when the immutable envelope reaches the spool.
// The standalone archive acknowledges independently after committing its data.
func (c *Collector) acceptCapture(ctx context.Context, source archive.Registration, previous sessioncapture.Checkpoint, batch sessioncapture.Batch) (finalErr error) {
	poll := capturePollFor(batch)
	defer func() {
		if finalErr != nil {
			delete(c.acceptedPolls, source.Key())
			return
		}
		if c.acceptedPolls == nil {
			c.acceptedPolls = map[string]capturePoll{}
		}
		c.acceptedPolls[source.Key()] = poll
	}()
	delivery, err := sessioncapture.NewDelivery(source.Source, previous, batch)
	if err != nil {
		return err
	}
	if last, ok := c.acceptedPolls[source.Key()]; ok && last == poll && previous.Version != 0 && previous == batch.Checkpoint && len(batch.Records) == 0 && len(batch.Gaps) == 0 {
		// Empty metadata envelopes can share a checkpoint while needing distinct
		// receipts. Never skip over an envelope whose acceptance is uncertain.
		drained := true
		if c.Spool != nil {
			pending, err := c.capturePending(source.Key())
			if err != nil {
				return err
			}
			drained = pending == 0
		}
		if drained {
			// Redaction and enrollment may change between identical observations.
			if err := c.Store.CheckCapture(ctx, source, batch); err != nil {
				return err
			}
			c.Metrics.Add("empty_polls_skipped", 1)
			return nil
		}
	}
	c.invalidateCaptureCheckpoint(source.Key())
	if c.Spool != nil {
		if err := c.Store.CheckCapture(ctx, source, batch); err != nil {
			return err
		}
		if err := c.Spool.Enqueue(delivery, []string{localDestination}); err != nil {
			return err
		}
		_, _ = c.Spool.Deliver(ctx, collectorSink{c}, 256)
		accepted, err := c.Spool.Acknowledged(delivery.BatchID, localDestination)
		if err != nil {
			return err
		}
		if !accepted {
			return errors.New("capture_delivery_pending")
		}
		return nil
	}
	sink, err := c.Store.CaptureSink(localDestination, source)
	if err != nil {
		return err
	}
	_, err = sessioncapture.Deliver(ctx, sink, delivery)
	return err
}

// Only inactive WAL bookkeeping timestamps are immaterial. An active timestamp
// refreshes durable liveness, and every pending/blocked/backfill edge is kept.
type capturePoll struct {
	checkpoint             sessioncapture.Checkpoint
	pending, blocked, more bool
	active                 bool
	activeAt               time.Time
}

func capturePollFor(batch sessioncapture.Batch) capturePoll {
	p := capturePoll{checkpoint: batch.Checkpoint, pending: batch.Pending, blocked: batch.Blocked, more: batch.More, active: batch.SourceActive}
	if batch.SourceActive {
		p.activeAt = batch.SourceActiveAt
	}
	return p
}

type walTick struct {
	at            time.Time
	count, maxPos int
	active        bool
}

type cancelReader struct {
	f   *os.File
	ctx context.Context
}

func (r cancelReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.f.Read(p)
}
func (r cancelReader) Seek(n int64, whence int) (int64, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.f.Seek(n, whence)
}

func captureFile(ctx context.Context, r archive.Registration, cp sessioncapture.Checkpoint) (sessioncapture.Batch, error) {
	return captureFileMeasured(ctx, r, cp, nil)
}

type measuredInput struct {
	io.ReadSeeker
	bytes int64
}

func (r *measuredInput) Read(p []byte) (int, error) {
	n, err := r.ReadSeeker.Read(p)
	r.bytes += int64(n)
	return n, err
}

func captureFileMeasured(ctx context.Context, r archive.Registration, cp sessioncapture.Checkpoint, metrics *observability.Recorder) (sessioncapture.Batch, error) {
	root, err := os.OpenRoot(r.Root)
	if err != nil {
		return sessioncapture.Batch{}, err
	}
	defer root.Close()
	f, err := root.OpenFile(r.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return sessioncapture.Batch{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return sessioncapture.Batch{}, err
	}
	if !before.Mode().IsRegular() {
		return sessioncapture.Batch{}, errors.New("regular_source_required")
	}
	var input io.ReadSeeker = cancelReader{f, ctx}
	var snapshot []byte
	if r.Provider == sessioncapture.Devin {
		// The caller deferred while the WAL was hot; it is quiet now, so the
		// canonical dump is a consistent snapshot.
		dump, err := sessioncapture.DevinDump(ctx, r.Root, r.Path)
		if err != nil {
			return sessioncapture.Batch{}, err
		}
		snapshot = dump
	}
	measured := &measuredInput{ReadSeeker: input}
	defer func() { metrics.Add("capture_input_bytes", measured.bytes) }()
	var batch sessioncapture.Batch
	if snapshot != nil {
		metrics.Add("capture_input_bytes", int64(len(snapshot)))
		batch, err = sessioncapture.ReadSnapshot(snapshot, r.Source, cp, sessioncapture.DefaultLimits(), time.Now())
	} else {
		batch, err = sessioncapture.Read(measured, r.Source, cp, sessioncapture.DefaultLimits(), time.Now())
	}
	if err != nil {
		return sessioncapture.Batch{}, err
	}
	after, err := root.Stat(r.Path)
	if err != nil || !os.SameFile(before, after) {
		return sessioncapture.Batch{}, errors.New("source_replaced_during_read")
	}
	return batch, nil
}

func (c *Collector) Run(ctx context.Context) {
	for {
		more := c.Scan(ctx)
		delay := time.Duration(c.Config.PollMilliseconds) * time.Millisecond
		if more {
			delay = 10 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
