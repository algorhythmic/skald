package sessioncapture

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscoveryConfinementAndNativeIdentity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()
	raw := `{"type":"file-history-snapshot"}` + "\n" + `{"type":"user","sessionId":"native-session","version":"2.1.263","cwd":"/project","message":{"role":"user","content":"Private text never returned in metadata"}}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "arbitrary-name.jsonl"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(outside, "outside.jsonl"), []byte(raw), 0600)
	os.Symlink(filepath.Join(outside, "outside.jsonl"), filepath.Join(root, "escape.jsonl"))
	os.Symlink(outside, filepath.Join(root, "escape-dir"))
	os.Mkdir(filepath.Join(root, "subagents"), 0700)
	os.WriteFile(filepath.Join(root, "subagents", "agent.jsonl"), []byte(raw), 0600)
	candidates, err := Inventory(ctx, root, Claude, time.Time{}, 100)
	if err != nil || len(candidates) != 1 {
		t.Fatal(candidates, err)
	}
	id, err := Identify(ctx, root, candidates[0].Path, Claude)
	if err != nil || id.ConversationID != "native-session" || id.Project != "/project" || id.ProviderVersion != "2.1.263" {
		t.Fatal(id, err)
	}
	if _, err := Identify(ctx, root, "escape.jsonl", Claude); err == nil {
		t.Fatal("escaping symlink read")
	}
	if _, err := Inventory(ctx, root, Claude, time.Time{}, 1); err == nil || err.Error() != "discovery_limit_exceeded" {
		t.Fatal("partial inventory passed", err)
	}
	if v, err := Inventory(ctx, root, Claude, time.Now().Add(time.Hour), 100); err != nil || len(v) != 0 {
		t.Fatal("cutoff ignored")
	}
	os.WriteFile(filepath.Join(root, "conflict.jsonl"), []byte(raw+strings.Replace(raw, "native-session", "different", 1)), 0600)
	if _, err := Identify(ctx, root, "conflict.jsonl", Claude); err == nil || err.Error() != "conversation_identity_mismatch" {
		t.Fatal("conflicting identity", err)
	}
	os.WriteFile(filepath.Join(root, "side.jsonl"), []byte(strings.Replace(raw, `"type":"user"`, `"isSidechain":true,"type":"user"`, 1)), 0600)
	if _, err := Identify(ctx, root, "side.jsonl", Claude); err == nil || err.Error() != "subagent_identity_unsupported" {
		t.Fatal("unproven subagent accepted", err)
	}
	os.WriteFile(filepath.Join(root, "pending.jsonl"), []byte(`{"type":"session_meta","payload":{"id":"codex-native","cli_version":"0.153.4","originator":"codex_work_desktop","cwd":"/project"}}`), 0600)
	if _, err := Identify(ctx, root, "pending.jsonl", Codex); err == nil {
		t.Fatal("incomplete metadata enrolled")
	}
	f, _ := os.OpenFile(filepath.Join(root, "pending.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n")
	f.Close()
	id, err = Identify(ctx, root, "pending.jsonl", Codex)
	if err != nil || id.ConversationID != "codex-native" || id.Originator != "codex_work_desktop" {
		t.Fatal(id, err)
	}
}
