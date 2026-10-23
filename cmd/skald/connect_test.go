package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/internal/daemon"
)

func TestConnectPreviewPersistsScopeAndDoesNotCollect(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "sources")
	os.Mkdir(root, 0700)
	raw := `{"type":"assistant","sessionId":"native","version":"2.1.263","message":{"role":"assistant","content":"PRIVATE CONTENT SENTINEL"}}` + "\n"
	os.WriteFile(filepath.Join(root, "session.jsonl"), []byte(raw), 0600)
	var out, stderr bytes.Buffer
	if err := run([]string{"sources", "--provider", "claude_code", "--root", root, "--since", "all"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "PRIVATE CONTENT SENTINEL") || !strings.Contains(out.String(), "native") {
		t.Fatal("preview leaked content or lost identity")
	}
	config := filepath.Join(dir, "config", "sources.json")
	args := []string{"connect", "--provider", "claude_code", "--root", root, "--config", config, "--namespace", "fixture:connect", "--since", "all", "--max-sessions", "12"}
	out.Reset()
	if err := run(args, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	json.Unmarshal(out.Bytes(), &receipt)
	if receipt["collecting"] != false {
		t.Fatal("connect claimed daemon was started")
	}
	initial, err := daemon.LoadConfig(config)
	if err != nil || len(initial.Discovery) != 1 {
		t.Fatal(initial, err)
	}
	info, _ := os.Stat(config)
	if info.Mode().Perm() != 0600 {
		t.Fatal("config isn't private")
	}
	out.Reset()
	if err := run([]string{"connect", "--provider", "claude_code", "--root", root, "--config", config}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	saved, err := daemon.LoadConfig(config)
	if err != nil || len(saved.Discovery) != 1 || saved.Discovery[0].Namespace != "fixture:connect" || saved.Discovery[0].MaxSessions != 12 || !saved.Discovery[0].ModifiedSince.IsZero() {
		t.Fatal("repeat connection changed saved scope", saved, err)
	}
	original, _ := os.ReadFile(config)
	if err := run(append(args, "--max-sessions", "4097"), &out, &stderr); err == nil {
		t.Fatal("invalid capacity accepted")
	}
	final, _ := os.ReadFile(config)
	if !bytes.Equal(original, final) {
		t.Fatal("failed update changed config")
	}
	link := filepath.Join(dir, "link.json")
	os.Symlink(config, link)
	if err := daemon.UpdateConfig(link, func(*daemon.Config) error { return nil }); err == nil {
		t.Fatal("symlink config overwritten")
	}
	if _, err := os.Stat(filepath.Join(dir, "archive.sqlite")); !os.IsNotExist(err) {
		t.Fatal("connect started collection")
	}
}

func TestConnectSubagentOptInPersistsAndCanBeRevoked(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "parent", "subagents", "agent-child.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw := `{"type":"user","isSidechain":true,"sessionId":"parent","agentId":"child","version":"2.1.303","message":{"role":"user","content":"PRIVATE CHILD SENTINEL"}}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	preview := []string{"sources", "--provider", "claude_code", "--root", root, "--since", "all"}
	if err := run(preview, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Candidates int `json:"candidates"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Candidates != 0 {
		t.Fatal("default preview included children", out.String(), err)
	}
	out.Reset()
	if err := run(append(preview, "--include-subagents"), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Candidates != 1 || strings.Contains(out.String(), "PRIVATE CHILD SENTINEL") {
		t.Fatal("bad child preview", out.String(), err)
	}
	config := filepath.Join(t.TempDir(), "sources.json")
	args := []string{"connect", "--provider", "claude_code", "--root", root, "--config", config, "--namespace", "fixture:children", "--since", "all", "--max-sessions", "4"}
	if err := run(append(args, "--include-subagents"), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := run(args, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	cfg, err := daemon.LoadConfig(config)
	if err != nil || !cfg.Discovery[0].IncludeSubagents {
		t.Fatal("repeat connection lost opt-in", cfg, err)
	}
	if err := run(append(args, "--include-subagents=false"), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	cfg, err = daemon.LoadConfig(config)
	if err != nil || cfg.Discovery[0].IncludeSubagents {
		t.Fatal("opt-out ignored", cfg, err)
	}
	if err := run([]string{"connect", "--provider", "codex", "--root", root, "--config", config, "--include-subagents"}, &out, &stderr); err == nil || err.Error() != "subagent_discovery_requires_claude" {
		t.Fatal("unsupported provider option accepted", err)
	}
	if err := daemon.UpdateConfig(config, func(c *daemon.Config) error {
		c.Discovery[0].Provider = "codex"
		c.Discovery[0].IncludeSubagents = true
		return nil
	}); err == nil {
		t.Fatal("unsupported config accepted")
	}
}

func TestConnectRaisesSavedCapacityWithinCombinedBound(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	config := filepath.Join(t.TempDir(), "sources.json")
	var out, stderr bytes.Buffer
	args := []string{"connect", "--provider", "codex", "--root", root, "--config", config, "--namespace", "fixture:large", "--since", "all"}
	for _, capacity := range []string{"128", "768"} {
		if err := run(append(args, "--max-sessions", capacity), &out, &stderr); err != nil {
			t.Fatal("capacity increase failed", capacity, err)
		}
	}
	if err := run(args, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	cfg, err := daemon.LoadConfig(config)
	if err != nil || len(cfg.Discovery) != 1 || cfg.Discovery[0].MaxSessions != 768 || cfg.Discovery[0].Namespace != "fixture:large" || !cfg.Discovery[0].ModifiedSince.IsZero() {
		t.Fatal("repeat connect changed enlarged root identity or scope", cfg, err)
	}
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"connect", "--provider", "claude_code", "--root", other, "--config", config, "--namespace", "fixture:other", "--max-sessions", "3500"}, &out, &stderr); err == nil || err.Error() != "configured_source_capacity_exceeded" {
		t.Fatal("combined capacity not enforced", err)
	}
	after, err := os.ReadFile(config)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed capacity increase changed saved config", err)
	}
}
