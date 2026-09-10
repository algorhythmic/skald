package daemon

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"time"

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
	Store           *archive.Store
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
	more := false
	for _, source := range c.Sources() {
		if ctx.Err() != nil {
			return false
		}
		cp, err := c.Store.Checkpoint(ctx, source.Key())
		if err != nil {
			c.issue(source.Key(), "checkpoint_read_failed")
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
		batch, err := captureFile(ctx, source, cp)
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
		if err := c.Store.Ingest(ctx, source, cp, batch, time.Now()); err != nil {
			code := "archive_write_failed"
			if errors.Is(err, archive.ErrCapacity) {
				code = "capture_capacity_limit"
			}
			if err.Error() == "capture_suppressed" {
				code = "capture_suppressed"
			}
			c.issue(source.Key(), code)
			_ = c.Store.SetHealth(ctx, source, "blocked", time.Now())
			continue
		}
		c.issue(source.Key(), "")
		if batch.Blocked {
			c.issue(source.Key(), "capture_blocked")
		}

		if !batch.More && !batch.Pending && !batch.Blocked && stampErr == nil && batch.Checkpoint.Offset == stamp.Size {
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
	batch, err := sessioncapture.Read(cancelReader{f, ctx}, r.Source, cp, sessioncapture.DefaultLimits(), time.Now())
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
