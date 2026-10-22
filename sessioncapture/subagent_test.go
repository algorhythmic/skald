package sessioncapture_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

func childFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../testdata/claude/subagent.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func childSource(id string) sessioncapture.Source {
	return sessioncapture.Source{Namespace: "fixture:claude_code", Provider: sessioncapture.Claude, StreamID: "child-stream", ConversationID: id, ProviderVersion: "2.1.303"}
}

func TestClaudeSubagentOptInDiscoveryAndIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join("projects", "synthetic-project", "synthetic-parent", "subagents", "agent-a1234567890abcde.jsonl")
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(path)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, path), childFixture(t), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, path), filepath.Join(root, "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if found, err := sessioncapture.Inventory(ctx, root, sessioncapture.Claude, time.Time{}, 100); err != nil || len(found) != 0 {
		t.Fatalf("default inventory included child: %+v, %v", found, err)
	}
	opts := sessioncapture.InventoryOptions{IncludeClaudeSubagents: true}
	found, err := sessioncapture.InventoryWithOptions(ctx, root, sessioncapture.Claude, time.Time{}, 100, opts)
	if err != nil || len(found) != 1 || found[0].Path != path {
		t.Fatalf("opt-in inventory: %+v, %v", found, err)
	}
	if _, err := sessioncapture.Identify(ctx, root, path, sessioncapture.Claude); err == nil || err.Error() != "subagent_identity_unsupported" {
		t.Fatalf("default identity enrolled child: %v", err)
	}
	id, err := sessioncapture.IdentifyWithOptions(ctx, root, path, sessioncapture.Claude, opts)
	want := sessionrecord.Key("claude-subagent", "synthetic-parent", "a1234567890abcde")
	if err != nil || id.ConversationID != want || id.ParentConversationID != "synthetic-parent" || id.NativeAgentID != "a1234567890abcde" || id.ProviderVersion != "2.1.303" || !sessioncapture.IsClaudeSubagentConversation(id.ConversationID) {
		t.Fatalf("native identity: %+v, %v", id, err)
	}
	if _, err := sessioncapture.IdentifyWithOptions(ctx, root, "link.jsonl", sessioncapture.Claude, opts); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := sessioncapture.InventoryWithOptions(ctx, root, sessioncapture.Claude, time.Time{}, 2, opts); err == nil || err.Error() != "discovery_limit_exceeded" {
		t.Fatalf("bounded walk failed: %v", err)
	}
	if _, err := sessioncapture.InventoryWithOptions(ctx, root, sessioncapture.Codex, time.Time{}, 100, opts); err == nil || err.Error() != "unsupported_discovery_option" {
		t.Fatalf("unsupported option accepted: %v", err)
	}
}

func TestClaudeSubagentRecordsAndDistinctNativeUUIDs(t *testing.T) {
	id := sessionrecord.Key("claude-subagent", "synthetic-parent", "a1234567890abcde")
	s := childSource(id)
	b := childFixture(t)
	out := read(t, b, s, sessioncapture.Checkpoint{})
	if out.Blocked || out.Pending || len(out.Records) != 3 || out.Checkpoint.ConversationID != id {
		t.Fatalf("child capture: %+v", out)
	}
	if _, err := sessioncapture.NewDelivery(s, sessioncapture.Checkpoint{}, out); err != nil {
		t.Fatalf("child records failed v1 delivery contract: %v", err)
	}
	parentScope := sessionrecord.Scope{Kind: "conversation", Key: sessionrecord.Conversation(s.Namespace, "synthetic-parent")}
	for _, item := range out.Records {
		r := item.Record
		if r.AdapterVersion != sessioncapture.ClaudeSubagentAdapterVersion || r.ConversationKey == nil || *r.ConversationKey != sessionrecord.Conversation(s.Namespace, id) || len(r.Relations) != 1 || r.Relations[0].Type != "native_subagent_of" || r.Relations[0].Target != parentScope {
			t.Fatalf("child relation: %+v", r)
		}
		var nativeParent, nativeAgent string
		var sidechain bool
		if json.Unmarshal(r.Extensions["native_parent_session_id"], &nativeParent) != nil || json.Unmarshal(r.Extensions["native_agent_id"], &nativeAgent) != nil || json.Unmarshal(r.Extensions["native_is_sidechain"], &sidechain) != nil || nativeParent != "synthetic-parent" || nativeAgent != "a1234567890abcde" || !sidechain {
			t.Fatalf("native evidence absent: %+v", r.Extensions)
		}
		wire, _ := json.Marshal(r)
		if _, err := sessioncapture.DecodeEnvelope(wire, item.Raw); err != nil {
			t.Fatal(err)
		}
	}
	if out.Records[1].Record.Kind != "opaque_record" || out.Records[1].Record.NativeKind != "attachment" {
		t.Fatal("unknown attachment was not preserved")
	}
	if string(out.Records[0].Record.Extensions["native_parent_uuid"]) != "null" {
		t.Fatal("native parentUuid metadata was lost")
	}
	primary := childSource("synthetic-parent")
	primary.StreamID = "primary"
	primaryRaw := []byte(`{"type":"user","uuid":"shared-native-uuid","sessionId":"synthetic-parent","message":{"role":"user","content":"Synthetic parent."}}` + "\n")
	parent := read(t, primaryRaw, primary, sessioncapture.Checkpoint{})
	if parent.Records[0].Record.RecordKey == out.Records[0].Record.RecordKey {
		t.Fatal("parent and child reused UUID collided")
	}
	siblingID := sessionrecord.Key("claude-subagent", "synthetic-parent", "a222222222222222")
	sibling := childSource(siblingID)
	sibling.StreamID = "sibling"
	siblingRaw := bytes.ReplaceAll(b[:bytes.IndexByte(b, '\n')+1], []byte("a1234567890abcde"), []byte("a222222222222222"))
	sib := read(t, siblingRaw, sibling, sessioncapture.Checkpoint{})
	if sib.Blocked || sib.Records[0].Record.RecordKey == out.Records[0].Record.RecordKey || sib.Records[0].Record.Relations[0].Target != parentScope {
		t.Fatal("sibling identity collided")
	}
}

func TestClaudeSubagentTailRestartRewriteAndConflicts(t *testing.T) {
	id := sessionrecord.Key("claude-subagent", "synthetic-parent", "a1234567890abcde")
	s := childSource(id)
	b := childFixture(t)
	firstLine := b[:bytes.IndexByte(b, '\n')+1]
	first := read(t, append(append([]byte{}, firstLine...), b[len(firstLine):len(firstLine)+20]...), s, sessioncapture.Checkpoint{})
	if first.Blocked || !first.Pending || len(first.Records) != 1 || first.Checkpoint.Offset != int64(len(firstLine)) {
		t.Fatalf("partial tail advanced: %+v", first)
	}
	restarted := read(t, b, s, first.Checkpoint)
	if restarted.Blocked || restarted.Pending || len(restarted.Records) != 2 || restarted.Checkpoint.Ordinal != 3 {
		t.Fatalf("restart: %+v", restarted)
	}
	for name, tail := range map[string]string{
		"primary":        `{"type":"assistant","sessionId":"synthetic-parent","message":{"role":"assistant","content":"wrong stream"}}` + "\n",
		"sibling":        `{"type":"assistant","sessionId":"synthetic-parent","isSidechain":true,"agentId":"a222222222222222","message":{"role":"assistant","content":"wrong child"}}` + "\n",
		"missing_marker": `{"type":"assistant","sessionId":"synthetic-parent","agentId":"a1234567890abcde","message":{"role":"assistant","content":"unproven child"}}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			mixed := read(t, append(append([]byte{}, firstLine...), []byte(tail)...), s, first.Checkpoint)
			if !mixed.Blocked || len(mixed.Records) != 0 || mixed.Checkpoint.Offset != first.Checkpoint.Offset {
				t.Fatalf("mixed child advanced checkpoint: %+v", mixed)
			}
		})
	}
	rewritten := bytes.Replace(b, []byte("Synthetic child request."), []byte("Synthetic revised child request."), 1)
	rewrite := read(t, rewritten, s, restarted.Checkpoint)
	if rewrite.Blocked || !rewrite.Checkpoint.HasGaps || rewrite.Checkpoint.Epoch != restarted.Checkpoint.Epoch+1 || len(rewrite.Records) != 3 || rewrite.Gaps[0].Code != "continuity_gap" {
		t.Fatalf("rewrite continuity: %+v", rewrite)
	}
	if rewrite.Records[0].Record.RecordKey != outKeyForFirst(t, b, s) {
		t.Fatal("native UUID changed key across revision")
	}
	if rewrite.Records[0].Record.SourceRevision == sessionrecord.Digest(firstLine) {
		t.Fatal("changed raw revision was not recorded")
	}
}

func TestClaudeSubagentUnsupportedIdentityShapes(t *testing.T) {
	root := t.TempDir()
	opts := sessioncapture.InventoryOptions{IncludeClaudeSubagents: true}
	first := strings.TrimSpace(string(childFixture(t)[:bytes.IndexByte(childFixture(t), '\n')]))
	cases := map[string]string{
		"missing_agent":    strings.Replace(first, `,"agentId":"a1234567890abcde"`, "", 1),
		"missing_marker":   strings.Replace(first, `,"isSidechain":true`, "", 1),
		"false_marker":     strings.Replace(first, `"isSidechain":true`, `"isSidechain":false`, 1),
		"string_marker":    strings.Replace(first, `"isSidechain":true`, `"isSidechain":"true"`, 1),
		"null_marker":      strings.Replace(first, `"isSidechain":true`, `"isSidechain":null`, 1),
		"missing_parent":   strings.Replace(first, `,"sessionId":"synthetic-parent"`, "", 1),
		"wrong_agent_type": strings.Replace(first, `"agentId":"a1234567890abcde"`, `"agentId":123`, 1),
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			path := name + ".jsonl"
			if err := os.WriteFile(filepath.Join(root, path), []byte(line+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := sessioncapture.IdentifyWithOptions(context.Background(), root, path, sessioncapture.Claude, opts); err == nil {
				t.Fatal("unsupported native identity enrolled")
			}
		})
	}
	for name, next := range map[string]string{
		"mixed_primary": `{"type":"assistant","sessionId":"synthetic-parent","message":{"role":"assistant","content":"primary"}}`,
		"mixed_sibling": strings.Replace(first, `"agentId":"a1234567890abcde"`, `"agentId":"a222222222222222"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			path := name + ".jsonl"
			if err := os.WriteFile(filepath.Join(root, path), []byte(first+"\n"+next+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := sessioncapture.IdentifyWithOptions(context.Background(), root, path, sessioncapture.Claude, opts); err == nil || err.Error() != "conversation_identity_mismatch" {
				t.Fatalf("mixed identity enrolled: %v", err)
			}
		})
	}
	if capability := sessioncapture.Probe(sessioncapture.Claude, "2.1.303").NativeSubagents; capability.State != "fixture_verified" {
		t.Fatalf("validated child schema not reported: %+v", capability)
	}
}

func TestClaudeChildOnlyPathsRequireNativeChildEvidence(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "subagents"), 0700); err != nil {
		t.Fatal(err)
	}
	primary := []byte(`{"type":"user","sessionId":"synthetic-parent","message":{"role":"user","content":"Synthetic primary."}}` + "\n")
	for _, path := range []string{filepath.Join("subagents", "misplaced.jsonl"), "agent-misplaced.jsonl"} {
		if err := os.WriteFile(filepath.Join(root, path), primary, 0600); err != nil {
			t.Fatal(err)
		}
		for _, opts := range []sessioncapture.InventoryOptions{{}, {IncludeClaudeSubagents: true}} {
			if _, err := sessioncapture.IdentifyWithOptions(context.Background(), root, path, sessioncapture.Claude, opts); err == nil || err.Error() != "subagent_identity_unsupported" {
				t.Fatalf("primary-shaped file enrolled at child locator %q: %v", path, err)
			}
		}
	}
	// Relocation changes only the locator. Native child evidence still proves the
	// same child identity when a saved stream moves to an ordinary filename.
	if err := os.WriteFile(filepath.Join(root, "relocated.jsonl"), childFixture(t), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := sessioncapture.Identify(context.Background(), root, "relocated.jsonl", sessioncapture.Claude); err == nil || err.Error() != "subagent_identity_unsupported" {
		t.Fatalf("default API accepted relocated child: %v", err)
	}
	id, err := sessioncapture.IdentifyWithOptions(context.Background(), root, "relocated.jsonl", sessioncapture.Claude, sessioncapture.InventoryOptions{IncludeClaudeSubagents: true})
	want := sessionrecord.Key("claude-subagent", "synthetic-parent", "a1234567890abcde")
	if err != nil || id.ConversationID != want {
		t.Fatalf("relocated child identity changed: %+v, %v", id, err)
	}
}

func TestClaudeSubagentIdentityFreeMetadataPrefixAndTail(t *testing.T) {
	root := t.TempDir()
	metadata := []byte(`{"type":"file-history-snapshot","message":"synthetic metadata"}` + "\n")
	child := childFixture(t)
	raw := append(append(append([]byte{}, metadata...), child...), metadata...)
	if err := os.WriteFile(filepath.Join(root, "agent-synthetic.jsonl"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	opts := sessioncapture.InventoryOptions{IncludeClaudeSubagents: true}
	id, err := sessioncapture.IdentifyWithOptions(context.Background(), root, "agent-synthetic.jsonl", sessioncapture.Claude, opts)
	if err != nil {
		t.Fatalf("metadata prefix concealed native child: %v", err)
	}
	out := read(t, raw, childSource(id.ConversationID), sessioncapture.Checkpoint{})
	if out.Blocked || len(out.Records) != 5 || out.Checkpoint.Ordinal != 5 {
		t.Fatalf("identity-free metadata blocked enrolled child: %+v", out)
	}
	for _, index := range []int{0, 4} {
		r := out.Records[index].Record
		if r.Kind != "opaque_record" || r.AdapterVersion != sessioncapture.ClaudeSubagentAdapterVersion || len(r.Relations) != 0 {
			t.Fatalf("unproven metadata gained relation or was dropped: %+v", r)
		}
	}
	bad := []byte(`{"type":"file-history-snapshot","isSidechain":"true"}` + "\n")
	broken := read(t, append(append([]byte{}, child...), bad...), childSource(id.ConversationID), sessioncapture.Checkpoint{})
	if !broken.Blocked || len(broken.Records) != 3 || broken.Checkpoint.Offset != int64(len(child)) || broken.Gaps[0].Code != "subagent_identity_unsupported" {
		t.Fatalf("malformed explicit marker was accepted: %+v", broken)
	}
}

func TestClaudeSubagentNativeControlRows(t *testing.T) {
	root := t.TempDir()
	fork := `{"type":"fork-context-ref","agentId":"a1234567890abcde","ref":"synthetic"}` + "\n"
	child := string(childFixture(t)[:bytes.IndexByte(childFixture(t), '\n')+1])
	controls := []string{
		`{"type":"api-request-shape","agentId":"a1234567890abcde","sessionId":"synthetic-parent"}` + "\n",
		`{"type":"api-request-blob","agentId":"a1234567890abcde","sessionId":"synthetic-parent"}` + "\n",
		`{"type":"api-request","agentId":"a1234567890abcde","sessionId":"synthetic-parent"}` + "\n",
	}
	raw := []byte(fork + child + strings.Join(controls, ""))
	path := "agent-synthetic.jsonl"
	if err := os.WriteFile(filepath.Join(root, path), raw, 0600); err != nil {
		t.Fatal(err)
	}
	opts := sessioncapture.InventoryOptions{IncludeClaudeSubagents: true}
	id, err := sessioncapture.IdentifyWithOptions(context.Background(), root, path, sessioncapture.Claude, opts)
	if err != nil {
		t.Fatalf("native control prefix blocked identity: %v", err)
	}
	out := read(t, raw, childSource(id.ConversationID), sessioncapture.Checkpoint{})
	if out.Blocked || len(out.Records) != 5 {
		t.Fatalf("native controls blocked capture: %+v", out)
	}
	for _, i := range []int{0, 2, 3, 4} {
		r := out.Records[i].Record
		if r.Kind != "opaque_record" || len(r.Relations) != 1 || r.Relations[0].Type != "native_subagent_of" {
			t.Fatalf("control lost child relation: %+v", r)
		}
	}
	sibling := strings.Replace(fork, "a1234567890abcde", "a222222222222222", 1)
	mixed := []byte(sibling + child)
	if err := os.WriteFile(filepath.Join(root, path), mixed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := sessioncapture.IdentifyWithOptions(context.Background(), root, path, sessioncapture.Claude, opts); err == nil || err.Error() != "conversation_identity_mismatch" {
		t.Fatalf("sibling control enrolled: %v", err)
	}
	blocked := read(t, mixed, childSource(id.ConversationID), sessioncapture.Checkpoint{})
	if !blocked.Blocked || blocked.Checkpoint.Offset != 0 || len(blocked.Records) != 0 {
		t.Fatalf("sibling control advanced child: %+v", blocked)
	}
	primary := []byte(`{"type":"api-request","sessionId":"synthetic-parent"}` + "\n" + child)
	blocked = read(t, primary, childSource(id.ConversationID), sessioncapture.Checkpoint{})
	if !blocked.Blocked || blocked.Checkpoint.Offset != 0 {
		t.Fatalf("primary control advanced child: %+v", blocked)
	}
	ordinary := "ordinary.jsonl"
	controlThenPrimary := []byte(controls[0] + `{"type":"user","sessionId":"synthetic-parent","message":{"role":"user","content":"Synthetic primary."}}` + "\n")
	if err := os.WriteFile(filepath.Join(root, ordinary), controlThenPrimary, 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []sessioncapture.InventoryOptions{{}, opts} {
		if _, err := sessioncapture.IdentifyWithOptions(context.Background(), root, ordinary, sessioncapture.Claude, mode); err == nil || err.Error() != "subagent_identity_unsupported" {
			t.Fatalf("control plus primary enrolled: %v", err)
		}
	}
}

func outKeyForFirst(t *testing.T, b []byte, s sessioncapture.Source) string {
	t.Helper()
	return read(t, b, s, sessioncapture.Checkpoint{}).Records[0].Record.RecordKey
}
