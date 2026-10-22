package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/capturespool"
	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

func TestChildDiscoverySpoolRestartOptOutAndRecovery(t *testing.T) {
	ctx := context.Background()
	c := discoveryCollector(t, 2)
	spool, err := capturespool.Open(filepath.Join(t.TempDir(), "spool"), []string{localDestination}, capturespool.Limits{MaxPending: 16, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	c.Spool = spool
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	root := c.Config.Discovery[0].Root
	ns := c.Config.Namespaces()
	sourceFixture(t, c, "parent.jsonl", "parent", "parent evidence")
	path := filepath.Join(root, "parent", "subagents", "agent-child.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	child := `{"type":"user","isSidechain":true,"sessionId":"parent","agentId":"child","uuid":"native-message","version":"2.1.303","timestamp":"2026-10-21T10:00:00Z","message":{"role":"user","content":"child evidence"}}` + "\n"
	if err := os.WriteFile(path, []byte(child), 0600); err != nil {
		t.Fatal(err)
	}
	forceDiscovery(c)
	if len(c.Sources()) != 1 {
		t.Fatal("child captured without opt-in")
	}
	c.Config.Discovery[0].IncludeSubagents = true
	forceDiscovery(c)
	if len(c.Sources()) != 2 {
		t.Fatal("child missing", c.DiscoveryStatus(), c.Issues())
	}
	childID := sessionrecord.Key("claude-subagent", "parent", "child")
	childKey := sessionrecord.Conversation(ns[0], childID)
	sessions, err := c.Store.Sessions(ctx, ns, "", 10)
	if err != nil || len(sessions.Items) != 2 {
		t.Fatal(sessions, err)
	}
	for _, item := range sessions.Items {
		if item.IsSubagent != (item.Key == childKey) {
			t.Fatal("child classification lost", item)
		}
	}
	refs, err := c.Store.Artifacts(ctx, ns, childKey, "", 10)
	if err != nil || len(refs.Items) != 1 {
		t.Fatal(refs, err)
	}
	ref := refs.Items[0]
	exact, err := c.Store.Get(ctx, ns, ref.Key, ref.Revision, ref.Adapter, false)
	if err != nil || exact.Record.AdapterVersion != sessioncapture.ClaudeSubagentAdapterVersion || len(exact.Record.Relations) != 1 || exact.Record.Relations[0].Target.Key != sessionrecord.Conversation(ns[0], "parent") {
		t.Fatal("child provenance lost", exact, err)
	}
	health, err := c.Store.Health(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range health {
		if h.CapturedOffset == int64(len(child)) && (h.LastSourceTime == nil || *h.LastSourceTime != "2026-10-21T10:00:00Z") {
			t.Fatal("child adapter lost source time", h)
		}
	}
	// Restart preserves producer positions and exact identities.
	restarted := &Collector{Store: c.Store, Spool: spool, Config: c.Config}
	if err := restarted.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	forceDiscovery(restarted)
	stats, _ := c.Store.Stats(ctx, ns)
	if stats["record_versions"] != int64(3) {
		t.Fatal("restart duplicated records", stats)
	}
	// Disabling the option stops child polling, including persisted enrollment,
	// and permits reducing the selected-source capacity to the primary count.
	restarted.Config.Discovery[0].IncludeSubagents = false
	restarted.Config.Discovery[0].MaxSessions = 1
	if err := restarted.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	second := strings.Replace(child, "native-message", "second-message", 1)
	if err := os.WriteFile(path, []byte(child+second), 0600); err != nil {
		t.Fatal(err)
	}
	forceDiscovery(restarted)
	stats, _ = c.Store.Stats(ctx, ns)
	if len(restarted.Sources()) != 1 || stats["record_versions"] != int64(3) {
		t.Fatal("opt-out still polled child", stats)
	}
	sessions, err = c.Store.Sessions(ctx, ns, "", 10)
	if err != nil || len(sessions.Items) != 2 {
		t.Fatal("opt-out removed archive", sessions, err)
	}
	for _, item := range sessions.Items {
		if item.Key == childKey && item.SourceHealth != "unavailable" {
			t.Fatal("child borrowed parent's health", item)
		}
	}
	restarted.Config.Discovery[0].IncludeSubagents = true
	restarted.Config.Discovery[0].MaxSessions = 2
	if err := restarted.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	forceDiscovery(restarted)
	stats, _ = c.Store.Stats(ctx, ns)
	if stats["record_versions"] != int64(4) || len(restarted.Issues()) != 0 {
		t.Fatal("re-enabled child failed to resume", stats, restarted.Issues())
	}
	status, err := spool.Status()
	if err != nil || status.Pending != 0 {
		t.Fatal("spool not drained", status, err)
	}
}
