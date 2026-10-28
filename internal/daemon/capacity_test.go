package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/sessioncapture"
)

func TestConfiguredEnrollmentCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sources.json")
	load := func(cfg Config) error {
		t.Helper()
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		_, err = LoadConfig(path)
		return err
	}
	cfg := DefaultConfig()
	cfg.Discovery = []DiscoveryRoot{{Namespace: "fixture:capacity", Provider: sessioncapture.Codex, Root: t.TempDir(), MaxSessions: archive.MaxEnrolledSources}}
	if err := load(cfg); err != nil {
		t.Fatal("bounded root capacity rejected", err)
	}
	cfg.Discovery[0].MaxSessions++
	if err := load(cfg); err == nil {
		t.Fatal("oversized root accepted")
	}
	cfg.Discovery[0].MaxSessions = archive.MaxEnrolledSources - 1
	cfg.Sources = []archive.Registration{{Source: sessioncapture.Source{Namespace: "fixture:explicit", Provider: sessioncapture.Codex, StreamID: "one"}, Root: t.TempDir(), Path: "one.jsonl"}}
	if err := load(cfg); err != nil {
		t.Fatal("combined boundary rejected", err)
	}
	cfg.Discovery[0].MaxSessions++
	if err := load(cfg); err == nil || err.Error() != "configured_source_capacity_exceeded" {
		t.Fatal("combined capacity not enforced", err)
	}
	cfg.Discovery = nil
	cfg.Sources = make([]archive.Registration, archive.MaxEnrolledSources)
	for i := range cfg.Sources {
		cfg.Sources[i] = archive.Registration{Source: sessioncapture.Source{Namespace: "fixture:explicit", Provider: sessioncapture.Codex, StreamID: fmt.Sprintf("s-%d", i)}, Root: "/synthetic", Path: fmt.Sprintf("%d.jsonl", i)}
	}
	if err := load(cfg); err != nil {
		t.Fatal("explicit capacity boundary rejected", err)
	}
	cfg.Sources = append(cfg.Sources, cfg.Sources[0])
	if err := load(cfg); err == nil || err.Error() != "invalid_configuration_limits" {
		t.Fatal("oversized explicit corpus accepted", err)
	}
}

func TestDiscoveryBeyondStatusBoundPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	const count = 270
	cfg := DefaultConfig()
	cfg.MinFreeBytes = 0
	cfg.Discovery = []DiscoveryRoot{{Namespace: "fixture:discovery", Provider: sessioncapture.Claude, Root: t.TempDir(), MaxSessions: count}}
	archiveDir := filepath.Join(t.TempDir(), "archive")
	store, err := archive.Open(archiveDir, cfg.Options())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c := &Collector{Store: store, Config: cfg}
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		sourceFixture(t, c, fmt.Sprintf("%03d.jsonl", i), fmt.Sprintf("conversation-%d", i), "synthetic")
	}
	for pass := 0; pass < 5; pass++ {
		c.lastDiscovery = time.Time{}
		c.discover(ctx)
		want := min((pass+1)*64, count)
		if len(c.Sources()) != want {
			t.Fatal("discovery exceeded probe bound or lost candidates", pass, len(c.Sources()), c.DiscoveryStatus())
		}
	}
	before := map[string]string{}
	for _, r := range c.Sources() {
		before[r.ConversationID] = r.Key()
		if err := os.Remove(filepath.Join(r.Root, r.Path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := archive.Open(archiveDir, cfg.Options())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := &Collector{Store: reopened, Config: c.Config}
	if err := restarted.Initialize(ctx); err != nil {
		t.Fatal("large saved corpus did not restart", err)
	}
	for _, r := range restarted.Sources() {
		if before[r.ConversationID] != r.Key() {
			t.Fatal("restart changed stream identity", r)
		}
	}
	if len(restarted.Sources()) != count {
		t.Fatal("missing native files lost enrollment", len(restarted.Sources()))
	}
	status, err := reopened.Status(ctx, c.Config.Namespaces())
	if err != nil || status["sources_total"] != int64(count) || status["sources_omitted"] != int64(0) || len(status["sources"].([]archive.SourceHealth)) != count {
		t.Fatal("large enrollment omitted selected source diagnostics", status, err)
	}
	restarted.Config.Discovery[0].MaxSessions--
	if err := restarted.Initialize(ctx); err == nil || err.Error() != "discovery_capacity_below_enrolled_count" {
		t.Fatal("capacity reduction forgot retained sources", err)
	}
}

func TestCapacityDiagnosticSurvivesProbeLimitAndRejection(t *testing.T) {
	c := discoveryCollector(t, 1)
	sourceFixture(t, c, "enrolled.jsonl", "enrolled", "synthetic")
	forceDiscovery(c)
	key := c.Sources()[0].Key()
	root := c.Config.Discovery[0].Root
	if err := os.Rename(filepath.Join(root, "enrolled.jsonl"), filepath.Join(root, "relocated.jsonl")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, "relocated.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 130; i++ {
		sourceFixture(t, c, fmt.Sprintf("new-%03d.jsonl", i), fmt.Sprintf("new-%d", i), "synthetic")
	}
	// An older candidate rejects after proven capacity failures. Neither that
	// rejection nor probe exhaustion may mask the capacity diagnostic.
	if err := os.WriteFile(filepath.Join(root, "incomplete.jsonl"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	middle := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(root, "incomplete.jsonl"), middle, middle); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 3; pass++ {
		forceDiscovery(c)
		if c.DiscoveryStatus()[0].State != "capacity_reached" || len(c.Sources()) != 1 {
			t.Fatal("capacity diagnostic masked or bound exceeded", pass, c.DiscoveryStatus())
		}
	}
	if c.Sources()[0].Key() != key || c.Sources()[0].Path != "relocated.jsonl" || c.DiscoveryStatus()[0].Rejected != 1 {
		t.Fatal("capacity probing starved relocation or hid rejection", c.Sources(), c.DiscoveryStatus())
	}
}
