package archive

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
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

func TestDiscoveryEnrollmentPreservesOtherHealthObservations(t *testing.T) {
	s, registration, _ := testStore(t)
	ctx := context.Background()
	registrations := make([]Registration, 32)
	for i := range registrations {
		registrations[i] = registration
		registrations[i].StreamID = fmt.Sprintf("existing:%d", i)
		registrations[i].Path = fmt.Sprintf("existing-%d.jsonl", i)
	}
	if err := s.Register(ctx, registrations); err != nil {
		t.Fatal(err)
	}
	for _, r := range registrations {
		if err := s.ObserveHealth(ctx, r, "available", testNow); err != nil {
			t.Fatal(err)
		}
	}
	beforeChecks := maps.Clone(s.healthChecks)
	newSource := registration
	newSource.StreamID, newSource.Path = "discovered", "discovered.jsonl"
	if err := s.Enroll(ctx, []Registration{newSource}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.healthChecks, beforeChecks) {
		t.Fatal("enrolling one source discarded unrelated health observations")
	}
	var before, after int64
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	for _, r := range registrations {
		if err := s.ObserveHealth(ctx, r, "available", testNow.Add(5*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := s.FlushHealthBatch(ctx); err != nil || count != 0 {
		t.Fatal("discovery forced early health heartbeats", count, err)
	}
	if err := s.db.QueryRow("SELECT total_changes()").Scan(&after); err != nil || after != before {
		t.Fatalf("unaffected health scan wrote SQLite after enrollment: %d -> %d: %v", before, after, err)
	}
	if info, err := os.Stat(filepath.Join(s.dir, "archive.sqlite-wal")); err != nil || info.Size() != 0 {
		t.Fatal("unaffected health scan committed WAL after enrollment", info, err)
	}
	// Discovery must not reset the due time either: normal heartbeats remain
	// due together, rather than becoming individual per-source transitions.
	for _, r := range registrations {
		if err := s.ObserveHealth(ctx, r, "available", testNow.Add(61*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := s.FlushHealthBatch(ctx); err != nil || count != len(registrations) {
		t.Fatal("normal grouped heartbeat schedule was lost", count, err)
	}
}

func TestEnrollmentInvalidatesOnlyCommittedChangedLocators(t *testing.T) {
	s, r, _ := testStore(t)
	ctx := context.Background()
	other := r
	other.StreamID, other.Path = "other", "other.jsonl"
	if err := s.Enroll(ctx, []Registration{other}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []Registration{r, other} {
		if err := s.ObserveHealth(ctx, item, "available", testNow); err != nil {
			t.Fatal(err)
		}
	}
	before := maps.Clone(s.healthChecks)
	newSource := r
	newSource.StreamID, newSource.Path = "new", "new.jsonl"
	invalid := r
	invalid.Root = t.TempDir() // A relocation requires an explicit known alias.
	if err := s.Enroll(ctx, []Registration{newSource, invalid}); err == nil {
		t.Fatal("invalid relocation accepted")
	}
	if !reflect.DeepEqual(s.healthChecks, before) {
		t.Fatal("failed enrollment invalidated committed health observations")
	}
	r.Path = "relocated.jsonl"
	if err := s.Enroll(ctx, []Registration{r}); err != nil {
		t.Fatal(err)
	}
	if _, found := s.healthChecks[r.Key()]; found {
		t.Fatal("updated locator retained stale availability cache")
	}
	if s.healthChecks[other.Key()] != before[other.Key()] {
		t.Fatal("updated locator invalidated another source")
	}
	var health string
	if err := s.db.QueryRow("SELECT health FROM stream_locators WHERE stream_key=?", r.Key()).Scan(&health); err != nil || health != "unknown" {
		t.Fatal("enrollment did not reset durable availability", health, err)
	}
	if err := s.ObserveHealth(ctx, r, "available", testNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT health FROM stream_locators WHERE stream_key=?", r.Key()).Scan(&health); err != nil || health != "available" {
		t.Fatal("old cache suppressed the new availability transition", health, err)
	}
	if err := s.Register(ctx, []Registration{r}); err != nil {
		t.Fatal(err)
	}
	if len(s.healthChecks) != 0 {
		t.Fatal("replacement registration retained selected or removed health cache")
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
