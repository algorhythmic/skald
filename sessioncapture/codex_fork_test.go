package sessioncapture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/algorhythmic/skald/sessionrecord"
)

// All metadata and content in these fixtures is synthetic.
func codexForkMeta(id, parent, source string) string {
	return fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"forked_from_id":%s,"cwd":%q,"originator":%q,"cli_version":%q,"source":%s}}`+"\n", id, parent, "/"+id, "origin-"+id, "version-"+id, source)
}

func codexForkFixture() string {
	return codexForkMeta("child", `"parent"`, `{"subagent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}`) +
		codexForkMeta("parent", `null`, `"exec"`) +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic copied or current history"}]}}` + "\n"
}

func identifyCodexFixture(t *testing.T, raw string) (Identity, error) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "unrelated-name.jsonl"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return Identify(context.Background(), root, "unrelated-name.jsonl", Codex)
}

func TestCodexForkDiscoveryPreservesChildMetadata(t *testing.T) {
	for _, source := range []string{`"cli"`, `"vscode"`, `"exec"`} {
		t.Run(source, func(t *testing.T) {
			raw := strings.Replace(codexForkFixture(), `"source":"exec"`, `"source":`+source, 1)
			id, err := identifyCodexFixture(t, raw)
			if err != nil || id.ConversationID != "child" || id.ParentConversationID != "parent" || id.Project != "/child" || id.Originator != "origin-child" || id.ProviderVersion != "version-child" {
				t.Fatalf("child identity replaced: %+v, %v", id, err)
			}
		})
	}
}

func TestCodexForkCaptureAndCheckpointUseChildScope(t *testing.T) {
	raw := []byte(codexForkFixture())
	source := Source{Namespace: "synthetic", Provider: Codex, StreamID: "fork", ConversationID: "child"}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	whole, err := Read(bytes.NewReader(raw), source, Checkpoint{}, DefaultLimits(), now)
	if err != nil || whole.Blocked || len(whole.Records) != 3 || len(whole.Gaps) != 0 {
		t.Fatalf("capture failed: %+v, %v", whole, err)
	}
	child := sessionrecord.Conversation(source.Namespace, "child")
	parent := sessionrecord.Conversation(source.Namespace, "parent")
	for i, captured := range whole.Records {
		r := captured.Record
		if r.ConversationKey == nil || *r.ConversationKey != child || r.OriginScope.Key != child || r.ProviderVersion != "version-child" || r.AdapterVersion != CodexForkAdapterVersion || r.Coverage.Kind != "unknown" {
			t.Fatalf("record %d lost stream identity: %+v", i, r)
		}
		if err := r.VerifyRaw(captured.Raw); err != nil {
			t.Fatal(err)
		}
	}
	if got := whole.Records[0].Record.Relations; len(got) != 1 || got[0].Type != "native_fork_of" || got[0].Target.Key != parent || got[0].Evidence != "native_forked_from_id" {
		t.Fatal("missing direct native fork relation", got)
	}
	copied := whole.Records[1].Record
	if len(copied.Relations) != 1 || copied.Relations[0].Target.Key != parent || copied.Relations[0].Evidence != "bounded_native_forked_from_id_prefix" || string(copied.Extensions["native_session_id"]) != `"parent"` || string(copied.Extensions["native_inherited_cwd"]) != `"/parent"` || len(copied.Extensions["native_cwd"]) != 0 {
		t.Fatal("copied header lost native identity or invented child relation", copied)
	}
	limits := DefaultLimits()
	limits.Records = 1
	var cp Checkpoint
	var records []Captured
	for i := 0; i < len(whole.Records); i++ {
		batch, err := Read(bytes.NewReader(raw), source, cp, limits, now)
		if err != nil || batch.Blocked || len(batch.Records) != 1 {
			t.Fatalf("checkpoint capture %d: %+v, %v", i, batch, err)
		}
		// A process restart gets only persisted Source/checkpoint and bytes.
		wire, _ := json.Marshal(batch.Checkpoint)
		if err := json.Unmarshal(wire, &cp); err != nil {
			t.Fatal(err)
		}
		records = append(records, batch.Records...)
	}
	if !reflect.DeepEqual(records, whole.Records) || cp != whole.Checkpoint || cp.ConversationID != "child" {
		t.Fatal("restart changed fork record identity")
	}
	// Also establish the native identity without a pre-enrolled Source ID.
	source.ConversationID = ""
	unregistered, err := Read(bytes.NewReader(raw), source, Checkpoint{}, DefaultLimits(), now)
	if err != nil || unregistered.Blocked || unregistered.Checkpoint.ConversationID != "child" {
		t.Fatal("first native header did not establish child", unregistered.Gaps, err)
	}
}

func TestCodexForkAncestryRequiresOrderedExplicitEdges(t *testing.T) {
	nested := codexForkMeta("child", `"parent"`, `"exec"`) + codexForkMeta("parent", `"ancestor"`, `"cli"`) + codexForkMeta("ancestor", `null`, `"vscode"`)
	if id, err := identifyCodexFixture(t, nested); err != nil || id.ConversationID != "child" || id.ParentConversationID != "parent" {
		t.Fatal("explicit ancestry rejected", id, err)
	}
	nestedSource := Source{Namespace: "synthetic", Provider: Codex, StreamID: "nested", ConversationID: "child"}
	limits := DefaultLimits()
	limits.Records = 1
	var nestedCP Checkpoint
	for i := 0; i < 3; i++ {
		batch, err := Read(bytes.NewReader([]byte(nested)), nestedSource, nestedCP, limits, time.Now())
		if err != nil || batch.Blocked || len(batch.Records) != 1 || batch.Checkpoint.ProviderVersion != "version-child" {
			t.Fatal("nested ancestry restart rejected", batch.Gaps, err)
		}
		if got := batch.Records[0].Record.Relations; len(got) != 1 || got[0].Target.Key != sessionrecord.Conversation("synthetic", "parent") {
			t.Fatal("nested ancestor replaced direct child parent", got)
		}
		nestedCP = batch.Checkpoint
	}
	cases := map[string]string{
		"unrelated":              codexForkMeta("child", `null`, `"exec"`) + codexForkMeta("parent", `null`, `"cli"`),
		"sibling":                codexForkFixture() + codexForkMeta("sibling", `"parent"`, `"exec"`),
		"skipped_parent":         codexForkMeta("child", `"parent"`, `"exec"`) + codexForkMeta("ancestor", `null`, `"cli"`),
		"self":                   codexForkMeta("child", `"child"`, `"exec"`),
		"cycle":                  codexForkMeta("child", `"parent"`, `"exec"`) + codexForkMeta("parent", `"child"`, `"exec"`),
		"changed_edge":           codexForkFixture() + codexForkMeta("child", `"other"`, `"exec"`),
		"contradictory_spawn":    codexForkMeta("child", `"parent"`, `{"subagent":{"thread_spawn":{"parent_thread_id":"other"}}}`),
		"invalid_parent":         codexForkMeta("child", `123`, `"exec"`),
		"session_id_is_not_fork": `{"type":"session_meta","payload":{"id":"child","session_id":"parent"}}` + "\n" + codexForkMeta("parent", `null`, `"cli"`),
	}
	source := Source{Namespace: "synthetic", Provider: Codex, StreamID: "fork", ConversationID: "child"}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := identifyCodexFixture(t, raw); err == nil || err.Error() != "conversation_identity_mismatch" {
				t.Fatal("conflict accepted in discovery", err)
			}
			batch, err := Read(bytes.NewReader([]byte(raw)), source, Checkpoint{}, DefaultLimits(), time.Now())
			if err != nil || !batch.Blocked || len(batch.Gaps) != 1 || batch.Gaps[0].Code != "conversation_identity_mismatch" {
				t.Fatal("conflict accepted in capture", batch.Gaps, err)
			}
		})
	}
	// A spawn relationship without fork evidence does not authorize copied IDs.
	spawn := codexForkMeta("child", `null`, `{"subagent":{"thread_spawn":{"parent_thread_id":"parent"}}}`)
	if _, err := identifyCodexFixture(t, spawn); err != nil {
		t.Fatal("independent spawned stream rejected", err)
	}
	if _, err := identifyCodexFixture(t, spawn+codexForkMeta("parent", `null`, `"cli"`)); err == nil {
		t.Fatal("spawn source alone authorized inherited identity")
	}
}

func TestCodexForkLineageBoundsAndLateConflicts(t *testing.T) {
	var raw strings.Builder
	for i := 0; i < maxCodexForkAncestors; i++ {
		raw.WriteString(codexForkMeta(fmt.Sprintf("id-%d", i), fmt.Sprintf("\"id-%d\"", i+1), `"exec"`))
	}
	raw.WriteString(codexForkMeta(fmt.Sprintf("id-%d", maxCodexForkAncestors), `null`, `"cli"`))
	if _, err := identifyCodexFixture(t, raw.String()); err != nil {
		t.Fatal("bounded ancestry rejected", err)
	}
	tooDeep := strings.Replace(raw.String(), `"id":"id-16","forked_from_id":null`, `"id":"id-16","forked_from_id":"id-17"`, 1)
	if _, err := identifyCodexFixture(t, tooDeep); err == nil || err.Error() != "identity_probe_limit" {
		t.Fatal("unbounded ancestry accepted", err)
	}
	tooDeepSource := Source{Namespace: "synthetic", Provider: Codex, StreamID: "deep", ConversationID: "id-0"}
	if batch, err := Read(bytes.NewReader([]byte(tooDeep)), tooDeepSource, Checkpoint{}, DefaultLimits(), time.Now()); err != nil || !batch.Blocked {
		t.Fatal("capture accepted unbounded ancestry", batch.Gaps, err)
	}
	// Discovery is intentionally bounded; ingestion must still block an unrelated
	// metadata ID after that prefix and after a persisted checkpoint boundary.
	prefix := codexForkFixture() + strings.Repeat(`{"type":"event_msg","payload":{"type":"synthetic"}}`+"\n", 256)
	source := Source{Namespace: "synthetic", Provider: Codex, StreamID: "fork", ConversationID: "child"}
	batch, err := Read(bytes.NewReader([]byte(prefix)), source, Checkpoint{}, DefaultLimits(), time.Now())
	if err != nil || batch.Blocked {
		t.Fatal(batch.Gaps, err)
	}
	tail := prefix + codexForkMeta("unrelated", `null`, `"cli"`)
	if _, err := identifyCodexFixture(t, tail); err != nil {
		t.Fatal("bounded discovery unexpectedly read tail", err)
	}
	blocked, err := Read(bytes.NewReader([]byte(tail)), source, batch.Checkpoint, DefaultLimits(), time.Now())
	if err != nil || !blocked.Blocked || blocked.Checkpoint != batch.Checkpoint || blocked.Gaps[0].Code != "conversation_identity_mismatch" {
		t.Fatal("late conflicting metadata consumed", blocked.Gaps, err)
	}
}

func TestCodexForkResumesCheckpointBeforeCopiedHeader(t *testing.T) {
	raw := []byte(codexForkFixture())
	header := raw[:bytes.IndexByte(raw, '\n')+1]
	source := Source{Namespace: "synthetic", Provider: Codex, StreamID: "fork", ConversationID: "child"}
	// This is the wire-compatible checkpoint the old adapter persisted after
	// consuming only the child's header and blocking at the copied parent's ID.
	old := Checkpoint{Version: 1, SourceKey: source.Key(), Generation: sessionrecord.Key("generation", source.Key(), "0", sessionrecord.Digest(header)),
		Offset: int64(len(header)), ParsedOffset: int64(len(header)), Ordinal: 1, PrefixDigest: sessionrecord.Digest(header), ConversationID: "child", ProviderVersion: "version-child"}
	batch, err := Read(bytes.NewReader(raw), source, old, DefaultLimits(), time.Now())
	if err != nil || batch.Blocked || len(batch.Records) != 2 || len(batch.Gaps) != 0 {
		t.Fatal("old partial checkpoint did not resume", batch.Gaps, err)
	}
	parent := sessionrecord.Conversation(source.Namespace, "parent")
	for _, captured := range batch.Records {
		relations := captured.Record.Relations
		if len(relations) != 1 || relations[0].Type != "native_fork_of" || relations[0].Target.Key != parent || relations[0].Evidence != "bounded_native_forked_from_id_prefix" {
			t.Fatal("resumed record lost prefix-proven fork relationship", relations)
		}
	}
}

func TestCodexForkAdapterCompatibilityIsVersionScoped(t *testing.T) {
	for _, version := range []string{"0.163.0", "future"} {
		c := Probe(Codex, version)
		want := "unsupported"
		if version == "0.163.0" {
			want = "fixture_verified"
		}
		if c.NativeForks.State != want || c.NativeSubagents.State != "unsupported" || c.ConversationAlias.State != "unsupported" {
			t.Fatal("fork capability overclaimed", c)
		}
	}
	// Existing primary output remains under its original adapter semantics.
	source := Source{Namespace: "synthetic", Provider: Codex, StreamID: "primary"}
	raw := []byte(codexForkMeta("primary", `null`, `"exec"`))
	batch, err := Read(bytes.NewReader(raw), source, Checkpoint{}, DefaultLimits(), time.Now())
	if err != nil || batch.Blocked || len(batch.Records) != 1 {
		t.Fatal(batch.Gaps, err)
	}
	r := batch.Records[0].Record
	if r.AdapterVersion != AdapterVersion || len(r.Relations) != 0 || len(r.Extensions["native_session_id"]) != 0 || len(r.Extensions["native_forked_from_id"]) != 0 {
		t.Fatal("primary adapter output changed", r)
	}
}

func TestCodexForkDoesNotEnrollAncestryBeyondPrefix(t *testing.T) {
	prefix := codexForkMeta("child", `"parent"`, `"exec"`) + strings.Repeat(`{"type":"event_msg","payload":{"type":"synthetic"}}`+"\n", 255)
	source := Source{Namespace: "synthetic", Provider: Codex, StreamID: "late", ConversationID: "child"}
	for _, test := range []struct {
		name, parent string
		blocked      bool
	}{
		{"explicit_terminal_parent", `null`, false},
		{"unbounded_new_edge", `"ancestor"`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte(prefix + codexForkMeta("parent", test.parent, `"cli"`))
			batch, err := Read(bytes.NewReader(raw), source, Checkpoint{}, DefaultLimits(), time.Now())
			if err != nil || batch.Blocked != test.blocked {
				t.Fatal("lineage prefix bound ignored", batch.Gaps, err)
			}
		})
	}
}
