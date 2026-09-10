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
	if err := run(append(args, "--max-sessions", "257"), &out, &stderr); err == nil {
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
