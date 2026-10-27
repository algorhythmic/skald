package archive

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
)

func TestHealthHeartbeatBatchAtomicRollbackAndRetry(t *testing.T) {
	ctx := context.Background()
	s, r, _ := testStore(t)
	registrations := make([]Registration, 615)
	for i := range registrations {
		registrations[i] = r
		registrations[i].StreamID = fmt.Sprintf("heartbeat-%03d", i)
	}
	if err := s.Register(ctx, registrations); err != nil {
		t.Fatal(err)
	}
	for _, source := range registrations {
		if err := s.ObserveHealth(ctx, source, "available", testNow); err != nil {
			t.Fatal(err)
		}
	}
	var before, after int64
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, source := range registrations {
		if err := s.ObserveHealth(ctx, source, "available", testNow.Add(90*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&after); err != nil || before != after {
		t.Fatalf("observations wrote individual heartbeats: %d -> %d (%v)", before, after, err)
	}
	keys := make([]string, len(registrations))
	for i, source := range registrations {
		keys[i] = source.Key()
	}
	slices.Sort(keys)
	// Fail the final update. Earlier source timestamps must also roll back:
	// this proves the whole scan shares one transaction rather than 615 commits.
	if _, err := s.db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_heartbeat BEFORE UPDATE OF checked_at ON stream_locators WHEN NEW.stream_key='%s' BEGIN SELECT RAISE(ABORT,'synthetic heartbeat failure'); END`, keys[len(keys)-1])); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHealth(ctx); err == nil {
		t.Fatal("heartbeat failure was hidden")
	} else {
		var heartbeatErr *HealthHeartbeatError
		if !errors.As(err, &heartbeatErr) || !slices.Equal(heartbeatErr.Keys, keys) || err.Error() != "health_heartbeat_failed" {
			t.Fatal("heartbeat failure lost affected keys or exposed SQL details", err)
		}
	}
	var unchanged int
	if err := s.db.QueryRow("SELECT count(*) FROM stream_locators WHERE checked_at=?", stamp(testNow)).Scan(&unchanged); err != nil || unchanged != len(registrations) {
		t.Fatal("failed batch partially committed", unchanged, err)
	}
	for _, source := range registrations {
		check := s.healthChecks[source.Key()]
		if check.persistedAt != testNow || check.checkedAt != testNow.Add(90*time.Second) {
			t.Fatal("rollback lost latest observation or advanced durability", check)
		}
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_heartbeat"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.FlushHealthBatch(ctx); err != nil || n != len(registrations) {
		t.Fatal("batch did not report one commit for all due sources", n, err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM stream_locators WHERE checked_at=?", stamp(testNow.Add(90*time.Second))).Scan(&unchanged); err != nil || unchanged != len(registrations) {
		t.Fatal("retry did not commit all heartbeats", unchanged, err)
	}
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHealth(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&after); err != nil || after != before {
		t.Fatal("drained heartbeat batch wrote again", before, after, err)
	}
}

func TestDeferredHealthTransitionAndCheckpointFreshness(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	before := oneHealth(t, s, r.Namespace)
	if err := s.ObserveHealth(ctx, r, "available", testNow.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	fresh := oneHealth(t, s, r.Namespace)
	if fresh.CheckedAt == nil || *fresh.CheckedAt != stamp(testNow.Add(90*time.Second)) || fresh.LastCapturedAt == nil || *fresh.LastCapturedAt != *before.LastCapturedAt || fresh.CapturedOffset != before.CapturedOffset {
		t.Fatal("check freshness changed record capture evidence", before, fresh)
	}
	if err := s.ObserveHealth(ctx, r, "unavailable", testNow.Add(91*time.Second)); err != nil {
		t.Fatal(err)
	}
	var health, at string
	if err := s.db.QueryRow("SELECT health,checked_at FROM stream_locators WHERE stream_key=?", r.Key()).Scan(&health, &at); err != nil || health != "unavailable" || at != stamp(testNow.Add(91*time.Second)) {
		t.Fatal("transition was deferred", health, at, err)
	}
	if err := s.FlushHealth(ctx); err != nil {
		t.Fatal(err)
	}
	if got := oneHealth(t, s, r.Namespace); got.Health != "unavailable" || *got.CheckedAt != at {
		t.Fatal("old heartbeat overwrote transition", got)
	}
}

func TestDeferredHealthBackupRestartAndCancellation(t *testing.T) {
	ctx := context.Background()
	s, r, _ := testStore(t)
	if err := s.ObserveHealth(ctx, r, "available", testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveHealth(ctx, r, "available", testNow.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.FlushHealth(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled flush did not report cancellation", err)
	}
	backup, err := s.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if err := Restore(ctx, backup, destination, s.options); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(destination, s.options)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if h := oneHealth(t, restored, r.Namespace); h.CheckedAt == nil || *h.CheckedAt != stamp(testNow) {
		t.Fatal("backup invented an uncommitted heartbeat", h)
	}
	if err := s.FlushHealth(context.WithoutCancel(canceled)); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveHealth(ctx, r, "available", testNow.Add(180*time.Second)); err != nil {
		t.Fatal(err)
	}
	dir, options := s.dir, s.options
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if h := oneHealth(t, reopened, r.Namespace); h.CheckedAt == nil || *h.CheckedAt != stamp(testNow.Add(90*time.Second)) {
		t.Fatal("restart lost committed heartbeat or invented the later uncommitted check", h)
	}
}
