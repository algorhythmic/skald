package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/algorhythmic/skald/capturespool"
	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/sessioncapture"
)

func stageCandidate(t *testing.T, c *Collector, source archive.Registration) captureCandidate {
	t.Helper()
	batch, err := captureFile(context.Background(), source, sessioncapture.Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := sourceStamp(source)
	if err != nil {
		t.Fatal(err)
	}
	return captureCandidate{source: source, batch: batch, stamp: stamp}
}
func twoStageSources(t *testing.T) (*Collector, []archive.Registration) {
	t.Helper()
	c, _ := collectorWithCountedSpool(t)
	first := c.Config.Sources[0]
	second := first
	second.Namespace = "fixture:second"
	second.StreamID = "second"
	sources := []archive.Registration{first, second}
	c.Config.Sources = sources
	if err := c.Store.Register(context.Background(), sources); err != nil {
		t.Fatal(err)
	}
	return c, sources
}
func TestCaptureStageWaitsForDurableArchiveAcknowledgement(t *testing.T) {
	c, sources := twoStageSources(t)
	ctx := context.Background()
	c.loadCaptureCheckpoints(ctx)
	stage := captureStage{collector: c}
	candidates := []captureCandidate{stageCandidate(t, c, sources[0]), stageCandidate(t, c, sources[1])}
	for _, candidate := range candidates {
		if staged, _ := stage.add(ctx, candidate); !staged {
			t.Fatal("small capture not staged")
		}
		if cp, found, err := c.Spool.Checkpoint(candidate.source.Key()); err != nil || found || cp.Version != 0 {
			t.Fatal("memory staging advanced durable producer", err)
		}
		if cp, err := c.Store.Checkpoint(ctx, candidate.source.Key()); err != nil || cp.Version != 0 {
			t.Fatal("memory staging advanced archive", err)
		}
		if c.checkpoints.invalid[candidate.source.Key()] || len(c.acceptedPolls) > 0 || len(c.verified) > 0 {
			t.Fatal("staged candidate marked successful")
		}
	}
	stage.flush(ctx)
	for _, candidate := range candidates {
		cp, err := c.Store.Checkpoint(ctx, candidate.source.Key())
		if err != nil || cp != candidate.batch.Checkpoint {
			t.Fatal("archive group missing checkpoint", err)
		}
		if _, ok := c.acceptedPolls[candidate.source.Key()]; !ok {
			t.Fatal("acknowledged observation missing")
		}
		if _, ok := c.verified[candidate.source.Key()]; !ok {
			t.Fatal("stable acknowledged source not cached")
		}
	}
	if status, _ := c.Spool.Status(); status.Pending != 0 {
		t.Fatal("group not acknowledged/pruned")
	}
	if len(stage.candidates) != 0 || stage.bytes != 0 {
		t.Fatal("flushed stage retained memory")
	}
	if c.Metrics.Snapshot().Counters["capture_group_batches"] != 2 {
		t.Fatal("group metrics did not count candidates")
	}
}
func TestCaptureStageChangedStampAndCancelledStage(t *testing.T) {
	for _, mode := range []string{"stamp", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := collectorWithCountedSpool(t)
			source := c.Config.Sources[0]
			candidate := stageCandidate(t, c, source)
			stage := captureStage{collector: c}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if staged, _ := stage.add(ctx, candidate); !staged {
				t.Fatal("candidate not staged")
			}
			if mode == "cancel" {
				cancel()
			} else {
				f, err := os.OpenFile(filepath.Join(source.Root, source.Path), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("\n")
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			stage.flush(ctx)
			if len(c.verified) != 0 {
				t.Fatal("changed/uncommitted source was cached")
			}
			cp, err := c.Store.Checkpoint(context.Background(), source.Key())
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" {
				if cp.Version != 0 || len(c.acceptedPolls) != 0 {
					t.Fatal("cancelled memory stage marked successful")
				}
				if status, _ := c.Spool.Status(); status.Pending != 0 {
					t.Fatal("cancelled stage enqueued")
				}
			} else if cp != candidate.batch.Checkpoint {
				t.Fatal("safe prefix not committed despite subsequent append")
			}
		})
	}
}
func TestCaptureStageSuppressionDoesNotBlockOtherSource(t *testing.T) {
	c, sources := twoStageSources(t)
	ctx := context.Background()
	stage := captureStage{collector: c}
	for _, source := range sources {
		stage.add(ctx, stageCandidate(t, c, source))
	}
	if _, err := c.Store.Redact(ctx, sources[0].Namespace, "source", sources[0].Namespace); err != nil {
		t.Fatal(err)
	}
	stage.flush(ctx)
	if cp, err := c.Store.Checkpoint(ctx, sources[0].Key()); err != nil || cp.Version != 0 {
		t.Fatal("suppressed candidate advanced", err)
	}
	if cp, err := c.Store.Checkpoint(ctx, sources[1].Key()); err != nil || cp.Version == 0 {
		t.Fatal("unrelated candidate blocked", err)
	}
	if c.Issues()[sources[0].Key()] != "capture_suppressed" {
		t.Fatal("suppression issue lost")
	}
}
func TestCaptureStageCapacityFallbackAndCooperativeAge(t *testing.T) {
	c, sources := twoStageSources(t)
	c.Spool.Close()
	spool, err := capturespool.Open(filepath.Join(t.TempDir(), "small-spool"), []string{localDestination}, capturespool.Limits{MaxPending: 1, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	c.Spool = spool
	stage := captureStage{collector: c}
	ctx := context.Background()
	for _, source := range sources {
		stage.add(ctx, stageCandidate(t, c, source))
	}
	stage.flush(ctx)
	if cp, err := c.Store.Checkpoint(ctx, sources[0].Key()); err != nil || cp.Version == 0 {
		t.Fatal("capacity fallback failed fitting candidate", err)
	}
	if cp, err := c.Store.Checkpoint(ctx, sources[1].Key()); err != nil || cp.Version != 0 {
		t.Fatal("capacity failure advanced other candidate", err)
	}
	if c.Metrics.Snapshot().Counters["capture_group_fallbacks"] != 1 || c.Issues()[sources[1].Key()] != "capture_capacity_limit" {
		t.Fatal("capacity fallback issue/metric missing")
	}
	// Age is tested without sleeping: the next bounded capture cooperatively
	// flushes an expired stage before adding another candidate.
	c2, ss := twoStageSources(t)
	aged := captureStage{collector: c2}
	aged.add(ctx, stageCandidate(t, c2, ss[0]))
	aged.started = time.Now().Add(-captureStageAge)
	aged.add(ctx, stageCandidate(t, c2, ss[1]))
	if len(aged.candidates) != 1 || aged.candidates[0].source.Key() != ss[1].Key() {
		t.Fatal("expired stage not flushed")
	}
	if cp, err := c2.Store.Checkpoint(ctx, ss[0].Key()); err != nil || cp.Version == 0 {
		t.Fatal("aged candidate not committed", err)
	}
	aged.flush(ctx)
}
