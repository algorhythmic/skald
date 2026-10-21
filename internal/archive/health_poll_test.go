package archive

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestIdleHealthChecksStayFreshWithoutPerPollWrites(t *testing.T) {
	s, r, _ := testStore(t)
	ctx := context.Background()
	if err := s.SetHealth(ctx, r, "available", testNow); err != nil {
		t.Fatal(err)
	}
	var before, after int64
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 60; i++ {
		if err := s.SetHealth(ctx, r, "available", testNow.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&after); err != nil || after != before {
		t.Fatalf("idle polling mutated SQLite: before=%d after=%d err=%v", before, after, err)
	}
	h := oneHealth(t, s, r.Namespace)
	if h.CheckedAt == nil || *h.CheckedAt != stamp(testNow.Add(59*time.Second)) {
		t.Fatal("live check freshness was lost", h)
	}
	var persisted string
	if err := s.db.QueryRow("SELECT checked_at FROM stream_locators WHERE stream_key=?", r.Key()).Scan(&persisted); err != nil || persisted != stamp(testNow) {
		t.Fatal("idle check rewrote durable heartbeat", persisted, err)
	}
	if err := s.SetHealth(ctx, r, "available", testNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT checked_at FROM stream_locators WHERE stream_key=?", r.Key()).Scan(&persisted); err != nil || persisted != stamp(testNow.Add(time.Minute)) {
		t.Fatal("periodic heartbeat was not persisted", persisted, err)
	}
	if err := s.SetHealth(ctx, r, "unavailable", testNow.Add(61*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT checked_at FROM stream_locators WHERE stream_key=? AND health='unavailable'", r.Key()).Scan(&persisted); err != nil || persisted != stamp(testNow.Add(61*time.Second)) {
		t.Fatal("health transition was delayed", persisted, err)
	}
}

func TestEnrollmentInvalidatesHealthObservationCache(t *testing.T) {
	s, r, _ := testStore(t)
	ctx := context.Background()
	if err := s.SetHealth(ctx, r, "available", testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(ctx, []Registration{r}); err != nil {
		t.Fatal(err)
	}
	if oneHealth(t, s, r.Namespace).Health != "unknown" {
		t.Fatal("configuration reload reused old availability")
	}
	if err := s.SetHealth(ctx, r, "available", testNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if oneHealth(t, s, r.Namespace).Health != "available" {
		t.Fatal("old cache prevented availability recovery")
	}
}

func TestBackupRetainsDurableHealthHeartbeat(t *testing.T) {
	s, r, _ := testStore(t)
	ctx := context.Background()
	if err := s.SetHealth(ctx, r, "available", testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHealth(ctx, r, "available", testNow.Add(30*time.Second)); err != nil {
		t.Fatal(err)
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
	h := oneHealth(t, restored, r.Namespace)
	if h.CheckedAt == nil || *h.CheckedAt != stamp(testNow) || h.Health != "available" {
		t.Fatal("restore invented the newer in-memory heartbeat", h)
	}
}
