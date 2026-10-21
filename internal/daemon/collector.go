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
	"github.com/algorhythmic/skald/sessioncapture"
)

type Collector struct {
	scanMu          sync.Mutex
	sources         []archive.Registration
	discovery       map[string]DiscoveryStatus
	lastDiscovery   time.Time
	discoveryCursor map[string]int
	verified        map[string]verifiedFile
	devinWal        map[string]walTick
	Store           *archive.Store
	Spool           *capturespool.Spool
	Config          Config
	mu              sync.Mutex
	issues          map[string]string
}

func (c *Collector) Issues() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for k, v := range c.issues {
		out[k] = v
	}
	return out
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
	c.discover(ctx)
	// Retry already captured envelopes even when the native source is offline.
	if c.Spool != nil {
		_, _ = c.Spool.Deliver(ctx, collectorSink{c}, 256)
	}
	more := false
	for _, source := range c.Sources() {
		if ctx.Err() != nil {
			return false
		}
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
			health := "available"
			if cp.HasGaps {
				health = "capture_gaps"
			}
			if err := c.Store.SetHealth(ctx, source, health, time.Now()); err != nil {
				c.issue(source.Key(), "archive_write_failed")
			} else {
				c.issue(source.Key(), "")
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
			batch, err = captureFile(ctx, source, cp)
		}
		if err != nil {
			code := "source_unavailable"
			if errors.Is(err, context.Canceled) {
				return false
			}
			if err.Error() == "source_changed_during_read" || err.Error() == "source_replaced_during_read" {
				code = "source_changed_during_read"
			}
			c.issue(source.Key(), code)
			_ = c.Store.SetHealth(ctx, source, "unavailable", time.Now())
			continue
		}
		if err := c.acceptCapture(ctx, source, cp, batch); err != nil {
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
			_ = c.Store.SetHealth(ctx, source, "blocked", time.Now())
			continue
		}
		c.issue(source.Key(), "")
		if batch.Blocked {
			c.issue(source.Key(), "capture_blocked")
		}

		consumed := batch.Checkpoint.Offset == stamp.Size
		if source.Provider == sessioncapture.Devin {
			// The canonical dump, not the file size, is the captured extent.
			consumed = true
		}
		if !batch.More && !batch.Pending && !batch.Blocked && stampErr == nil && consumed {
			if after, err := sourceStamp(source); err == nil && after == stamp {
				if c.verified == nil {
					c.verified = map[string]verifiedFile{}
				}
				c.verified[source.Key()] = verifiedFile{Stamp: stamp, Checkpoint: batch.Checkpoint, At: time.Now()}
			}
		}
		more = more || batch.More
	}
	return more
}

// A producer checkpoint advances when the immutable envelope reaches the spool.
// The standalone archive acknowledges independently after committing its data.
func (c *Collector) acceptCapture(ctx context.Context, source archive.Registration, previous sessioncapture.Checkpoint, batch sessioncapture.Batch) error {
	delivery, err := sessioncapture.NewDelivery(source.Source, previous, batch)
	if err != nil {
		return err
	}
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
	if r.Provider == sessioncapture.Devin {
		// The caller deferred while the WAL was hot; it is quiet now, so the
		// canonical dump is a consistent snapshot.
		dump, err := sessioncapture.DevinDump(ctx, r.Root, r.Path)
		if err != nil {
			return sessioncapture.Batch{}, err
		}
		input = bytes.NewReader(dump)
	}
	batch, err := sessioncapture.Read(input, r.Source, cp, sessioncapture.DefaultLimits(), time.Now())
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
