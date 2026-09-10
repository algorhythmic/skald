package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"skald/internal/archive"
	"skald/sessioncapture"
)

func discoveryCollector(t *testing.T, max int) *Collector {
	t.Helper()
	cfg := DefaultConfig()
	cfg.MinFreeBytes = 0
	cfg.Discovery = []DiscoveryRoot{{Namespace: "fixture:discovery", Provider: sessioncapture.Claude, Root: t.TempDir(), MaxSessions: max}}
	s, err := archive.Open(filepath.Join(t.TempDir(), "archive"), cfg.Options())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &Collector{Store: s, Config: cfg}
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}
func sourceFixture(t *testing.T, c *Collector, path, id, text string) {
	t.Helper()
	raw := fmt.Sprintf("{\"type\":\"file-history-snapshot\"}\n{\"type\":\"assistant\",\"sessionId\":%q,\"uuid\":\"native-message\",\"version\":\"2.1.263\",\"message\":{\"role\":\"assistant\",\"content\":%q}}\n", id, text)
	if err := os.WriteFile(filepath.Join(c.Config.Discovery[0].Root, path), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}
func forceDiscovery(c *Collector) { c.lastDiscovery = time.Time{}; c.Scan(context.Background()) }
func TestDiscoveryRelocationAtCapacity(t *testing.T) {
	c := discoveryCollector(t, 1)
	root := c.Config.Discovery[0].Root
	sourceFixture(t, c, "one.jsonl", "one", "original")
	forceDiscovery(c)
	key := c.Sources()[0].Key()
	if err := os.Rename(filepath.Join(root, "one.jsonl"), filepath.Join(root, "moved.jsonl")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, "moved.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	// A newer conversation cannot take a slot or hide the enrolled file's move.
	sourceFixture(t, c, "new.jsonl", "new", "another conversation")
	forceDiscovery(c)
	sources := c.Sources()
	if len(sources) != 1 || sources[0].Key() != key || sources[0].Path != "moved.jsonl" {
		t.Fatal("capacity prevented relocation", sources, c.DiscoveryStatus())
	}
	if c.DiscoveryStatus()[0].State != "capacity_reached" {
		t.Fatal(c.DiscoveryStatus())
	}
}
func TestDiscoveryEnrollmentRestartMovesLossAndScope(t *testing.T) {
	ctx := context.Background()
	c := discoveryCollector(t, 4)
	root := c.Config.Discovery[0].Root
	ns := c.Config.Namespaces()
	sourceFixture(t, c, "one.jsonl", "conversation-one", "original")
	forceDiscovery(c)
	sources := c.Sources()
	if len(sources) != 1 || sources[0].ConversationID != "conversation-one" {
		t.Fatal(sources, c.DiscoveryStatus())
	}
	key := sources[0].Key()
	cp, err := c.Store.Checkpoint(ctx, key)
	if err != nil || cp.Ordinal != 2 {
		t.Fatal("metadata prefix was not captured with proven identity", cp, err)
	}
	sourceFixture(t, c, "two.jsonl", "conversation-two", "second")
	forceDiscovery(c)
	if len(c.Sources()) != 2 {
		t.Fatal("new file not enrolled")
	}
	restarted := &Collector{Store: c.Store, Config: c.Config}
	if err := restarted.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	forceDiscovery(restarted)
	persisted, _ := c.Store.Checkpoint(ctx, key)
	if persisted != cp {
		t.Fatal("restart changed source identity/checkpoint")
	}
	raw, _ := os.ReadFile(filepath.Join(root, "one.jsonl"))
	os.WriteFile(filepath.Join(root, "duplicate.jsonl"), raw, 0600)
	forceDiscovery(restarted)
	if len(restarted.Sources()) != 2 || restarted.DiscoveryStatus()[0].Rejected != 1 {
		t.Fatal("duplicate locator silently merged", restarted.DiscoveryStatus())
	}
	os.Remove(filepath.Join(root, "duplicate.jsonl"))
	os.Rename(filepath.Join(root, "one.jsonl"), filepath.Join(root, "moved.jsonl"))
	forceDiscovery(restarted)
	found := false
	for _, r := range restarted.Sources() {
		if r.Key() == key {
			found = r.Path == "moved.jsonl"
		}
	}
	after, _ := c.Store.Checkpoint(ctx, key)
	if !found || after != cp {
		t.Fatal("verified move changed stream", after)
	}
	os.Remove(filepath.Join(root, "moved.jsonl"))
	sourceFixture(t, restarted, "unproven.jsonl", "conversation-one", "changed prefix")
	forceDiscovery(restarted)
	if restarted.DiscoveryStatus()[0].Issues["unproven.jsonl"] != "relocation_prefix_mismatch" {
		t.Fatal("unproven move accepted", restarted.DiscoveryStatus())
	}
	sessions, err := c.Store.Sessions(ctx, ns, "", 25)
	if err != nil || len(sessions.Items) != 2 {
		t.Fatal("source loss deleted archive")
	}
	empty := &Collector{Store: c.Store, Config: DefaultConfig()}
	if err := empty.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	hidden, _ := c.Store.Sessions(ctx, empty.Config.Namespaces(), "", 25)
	if len(hidden.Items) != 0 {
		t.Fatal("removed scope still visible")
	}
	preserved, _ := c.Store.Sessions(ctx, ns, "", 25)
	if len(preserved.Items) != 2 {
		t.Fatal("scope removal deleted archive")
	}
}
func TestDiscoveryCapacityProbeFairnessAndIdleVerification(t *testing.T) {
	ctx := context.Background()
	c := discoveryCollector(t, 1)
	root := c.Config.Discovery[0].Root
	sourceFixture(t, c, "valid.jsonl", "one", "same size A")
	// Many newer incomplete files must not permanently starve an older valid file.
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(root, "valid.jsonl"), old, old)
	for i := 0; i < 65; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("pending-%02d.jsonl", i)), []byte("{"), 0600)
	}
	forceDiscovery(c)
	if c.DiscoveryStatus()[0].State != "probe_limit" {
		t.Fatal(c.DiscoveryStatus())
	}
	forceDiscovery(c)
	if len(c.Sources()) != 1 {
		t.Fatal("pending files starved a valid source", c.DiscoveryStatus())
	}
	r := c.Sources()[0]
	cached := c.verified[r.Key()]
	if cached.At.IsZero() {
		t.Fatal("complete file not cached")
	}
	c.Scan(ctx)
	if !c.verified[r.Key()].At.Equal(cached.At) {
		t.Fatal("idle poll reread unchanged content")
	}
	path := filepath.Join(root, r.Path)
	before, _ := os.Stat(path)
	raw, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(raw), "same size A", "same size B", 1)), 0600)
	os.Chtimes(path, before.ModTime(), before.ModTime())
	c.Scan(ctx)
	cp, _ := c.Store.Checkpoint(ctx, r.Key())
	if cp.Epoch != cached.Checkpoint.Epoch+1 {
		t.Fatal("same-size preserved-mtime rewrite missed")
	}
	value := c.verified[r.Key()]
	value.At = time.Now().Add(-2 * time.Minute)
	c.verified[r.Key()] = value
	c.Scan(ctx)
	if c.verified[r.Key()].At.Equal(value.At) {
		t.Fatal("periodic prefix audit skipped")
	}
	for i := 0; i < 65; i++ {
		os.Remove(filepath.Join(root, fmt.Sprintf("pending-%02d.jsonl", i)))
	}
	sourceFixture(t, c, "new.jsonl", "two", "new")
	forceDiscovery(c)
	if len(c.Sources()) != 1 || c.DiscoveryStatus()[0].State != "capacity_reached" {
		t.Fatal("capacity silently exceeded", c.DiscoveryStatus())
	}
}
