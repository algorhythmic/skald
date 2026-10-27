package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func testFetch(t *testing.T) (func(context.Context, string, url.Values) (any, error), *[]string) {
	t.Helper()
	requests := &[]string{}
	fetch := func(_ context.Context, path string, q url.Values) (any, error) {
		*requests = append(*requests, path+"?"+q.Encode())
		if q.Get("namespace") != "fixture:one" || len(q["namespace"]) != 1 {
			return nil, errors.New("scope_denied")
		}
		var wire string
		switch path {
		case "/v1/status":
			wire = `{"archive_instance":"instance-1","boundary":42,"sessions":1,"record_versions":3,"sources_total":1,"sources_omitted":0,"sources":[{"namespace":"fixture:one","provider":"claude_code","health":"available","checked_at":"now","stream_key":"private/path"}],"discovery":[{"root":"/secret/path"}]}`
		case "/v1/capabilities":
			wire = `{"archive_instance":"instance-1","namespaces":["fixture:one"],"features":["exact_revision_reads"]}`
		case "/v1/sessions":
			wire = `{"items":[{"conversation_key":"conversation-1","namespace":"fixture:one","title":"Example","title_ref":{"record_key":"title-record","source_revision":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}],"boundary":42}`
		case "/v1/transcript":
			wire = `{"items":[{"record_key":"r1","source_revision":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","role":"system","kind":"message","native_kind":"response_item/message","text":"secret instructions"},{"record_key":"r2","source_revision":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","role":"system","kind":"native_recap","native_kind":"system/away_summary","text":"native recap"},{"record_key":"r3","source_revision":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","role":"assistant","kind":"message","native_kind":"message","text":"answer"},{"record_key":"r4","source_revision":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","role":"developer","kind":"message","native_kind":"developer_note","text":"do not surface"},{"record_key":"r5","source_revision":"sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","role":"developer","kind":"native_note","native_kind":"plan","text":"do not surface either"},{"record_key":"r6","source_revision":"sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","role":"assistant","kind":"native_note","native_kind":"agent_thought","text":"assistant note"}],"boundary":42}`
		case "/v1/record":
			wire = `{"record":{"source_revision":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","adapter_version":"v1","body":{"text":"quoted evidence"}},"boundary":42}`
		case "/v1/changes":
			wire = `{"version":1,"phase":"tail","high_water":42,"events":[],"next_cursor":"next"}`
		default:
			return nil, errors.New("not_found")
		}
		var value any
		if err := json.Unmarshal([]byte(wire), &value); err != nil {
			t.Fatal(err)
		}
		return value, nil
	}
	return fetch, requests
}

func transact(t *testing.T, cfg Config, fetch func(context.Context, string, url.Values) (any, error), messages ...string) []map[string]any {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.fetch = fetch
	var output bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(messages, "\n")+"\n"), &output); err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("non-JSON stdout %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

const initMessage = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
const initializedMessage = `{"jsonrpc":"2.0","method":"notifications/initialized"}`

func call(id int, name, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args)
}
func structured(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	result, ok := row["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing tool result: %v", row)
	}
	payload, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("missing structured content: %v", result)
	}
	if _, ok := payload["provenance"].(map[string]any); !ok {
		t.Fatalf("missing provenance: %v", payload)
	}
	return payload
}

func TestStdioLifecycleToolsAndScopedReads(t *testing.T) {
	fetch, requests := testFetch(t)
	rev := "sha256:" + strings.Repeat("a", 64)
	rows := transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, fetch,
		initMessage, initializedMessage,
		`{"jsonrpc":"2.0","method":"notifications/unknown"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		call(4, "status", `{}`),
		call(5, "sessions.list", `{}`),
		call(6, "transcript.read", `{"conversation":"conversation-1"}`),
		call(7, "artifacts.get", fmt.Sprintf(`{"record":"r1","revision":%q,"adapter":"v1"}`, rev)),
		call(8, "changes.read", `{}`),
	)
	if len(rows) != 8 {
		t.Fatalf("expected 8 responses, got %d: %v", len(rows), rows)
	}
	initResult := rows[0]["result"].(map[string]any)
	if initResult["protocolVersion"] != ProtocolVersion {
		t.Fatalf("bad negotiation: %v", initResult)
	}
	listed := rows[2]["result"].(map[string]any)["tools"].([]any)
	if len(listed) != 9 {
		t.Fatalf("listed tools: %v", listed)
	}
	status := structured(t, rows[3])
	if strings.Contains(fmt.Sprint(status), "/secret/path") || strings.Contains(fmt.Sprint(status), "private/path") {
		t.Fatalf("status leaked source internals: %v", status)
	}
	prov := status["provenance"].(map[string]any)
	if prov["producer"] != "skald" || prov["authority"] != "conversation_evidence" || prov["archive_instance"] != "instance-1" || prov["archive_boundary"] != float64(42) {
		t.Fatalf("bad provenance: %v", prov)
	}
	session := structured(t, rows[4])["data"].(map[string]any)["items"].([]any)[0].(map[string]any)
	if p, ok := session["provenance"].(map[string]any); !ok || p["source_revision"] != "unknown" || session["title_ref"].(map[string]any)["source_revision"] == "" {
		t.Fatalf("aggregate session was attributed to title revision: %v", session)
	}
	transcript := structured(t, rows[5])["data"].(map[string]any)
	items := transcript["items"].([]any)
	if len(items) != 3 || transcript["omitted_instruction_blocks"] != float64(3) || items[0].(map[string]any)["evidence_class"] != "native_recap" || items[2].(map[string]any)["evidence_class"] != "native_note" {
		t.Fatalf("instruction filtering/recap failed: %v", transcript)
	}
	if got := structured(t, rows[6])["provenance"].(map[string]any)["source_revision"]; got != rev {
		t.Fatalf("exact revision missing: %v", got)
	}
	changes := structured(t, rows[7])
	if changes["provenance"].(map[string]any)["archive_boundary"] != "unknown" {
		t.Fatalf("feed high-water misreported as archive boundary: %v", changes)
	}
	for _, request := range *requests {
		u, _ := url.Parse(request)
		if u.Query().Get("namespace") != "fixture:one" {
			t.Fatalf("unscoped daemon call: %s", request)
		}
	}
}

func TestStdioErrorsBoundsAndGrant(t *testing.T) {
	fetch, requests := testFetch(t)
	rows := transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one", "fixture:two"}}, fetch,
		`not json`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		initMessage, initializedMessage,
		`{"jsonrpc":"2.0","id":2,"method":"does/not/exist"}`,
		call(3, "sessions.list", `{}`),
		call(4, "sessions.list", `{"namespace":"fixture:escape"}`),
		call(5, "artifacts.get", `{"namespace":"fixture:one","record":"r1","revision":"bad","adapter":"v1","raw":true}`),
		call(6, "sessions.list", `{"namespace":"fixture:one","limit":999}`),
		call(7, "sessions.list", `{"namespace":"fixture:one","unknown":true}`),
		call(9, "sessions.list", `{"namespace":null}`),
		call(10, "sessions.list", `{"namespace":"fixture:one","limit":null}`),
		call(11, "transcript.read", `{"namespace":"fixture:one","conversation":"c","history":null}`),
		call(12, "sessions.list", `{"namespace":"fixture:one","limit":0}`),
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"status","arguments":{}}}`,
		strings.Repeat("x", maxInput+1),
		call(8, "changes.read", `{"namespace":"fixture:one"}`),
	)
	if len(rows) != 15 {
		t.Fatalf("notifications/oversize response count: %d: %v", len(rows), rows)
	}
	if rows[0]["error"].(map[string]any)["code"] != float64(-32700) || rows[1]["error"].(map[string]any)["message"] != "not_initialized" || rows[3]["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("protocol errors: %v", rows)
	}
	for i, code := range []string{"namespace_required", "scope_denied", "invalid_arguments", "invalid_arguments", "invalid_arguments", "invalid_arguments", "invalid_arguments", "invalid_arguments", "invalid_arguments"} {
		payload := structured(t, rows[i+4])
		if payload["error"] != code || rows[i+4]["result"].(map[string]any)["isError"] != true {
			t.Fatalf("tool error %d: %v", i, rows[i+4])
		}
	}
	if rows[13]["error"].(map[string]any)["message"] != "message_too_large" {
		t.Fatalf("oversize handling: %v", rows[13])
	}
	_ = structured(t, rows[14])
	if len(*requests) != 3 { // final call: status, changes, instance verification
		t.Fatalf("invalid calls reached daemon: %v", *requests)
	}
}

func TestDaemonErrorStillHasProvenance(t *testing.T) {
	fetch, _ := testFetch(t)
	rows := transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, fetch, initMessage, initializedMessage, call(2, "artifacts.list", `{"conversation":"missing"}`))
	payload := structured(t, rows[1])
	if payload["error"] != "not_found" || rows[1]["result"].(map[string]any)["isError"] != true {
		t.Fatalf("missing tool error/provenance: %v", rows[1])
	}
}

func TestScopeRevocationAndBoundedOutput(t *testing.T) {
	baseFetch, requests := testFetch(t)
	revoked := func(_ context.Context, _ string, _ url.Values) (any, error) {
		return nil, errors.New("scope_denied")
	}
	rows := transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, revoked, initMessage, initializedMessage, call(2, "status", `{}`))
	payload := structured(t, rows[1])
	if payload["error"] != "scope_denied" || payload["provenance"].(map[string]any)["configured_scope"].([]any)[0] != "fixture:one" {
		t.Fatalf("revoked scope result: %v", payload)
	}
	largeFetch := func(ctx context.Context, path string, q url.Values) (any, error) {
		if path == "/v1/sessions" {
			return map[string]any{"items": []any{map[string]any{"title": strings.Repeat("x", maxOutput)}}, "boundary": float64(42)}, nil
		}
		return baseFetch(ctx, path, q)
	}
	rows = transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, largeFetch, initMessage, initializedMessage, call(2, "sessions.list", `{}`))
	payload = structured(t, rows[1])
	if payload["error"] != "response_too_large" {
		t.Fatalf("large output was not bounded: %v", payload["error"])
	}
	if len(*requests) != 2 {
		t.Fatalf("unexpected read calls: %v", *requests)
	}
}

func TestArchiveInstanceRaceDoesNotMisattribute(t *testing.T) {
	baseFetch, _ := testFetch(t)
	reads := 0
	race := func(ctx context.Context, path string, q url.Values) (any, error) {
		value, err := baseFetch(ctx, path, q)
		if path == "/v1/status" {
			reads++
			if reads == 2 {
				value.(map[string]any)["archive_instance"] = "instance-2"
			}
		}
		return value, err
	}
	rows := transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, race, initMessage, initializedMessage, call(2, "sessions.list", `{}`))
	payload := structured(t, rows[1])
	if payload["error"] != "archive_instance_changed" || payload["provenance"].(map[string]any)["archive_instance"] != "unknown" {
		t.Fatalf("raced archive was misattributed: %v", payload)
	}
}

func TestArchiveBoundaryComesFromDataRead(t *testing.T) {
	baseFetch, _ := testFetch(t)
	checks := 0
	advanced := func(ctx context.Context, path string, q url.Values) (any, error) {
		value, err := baseFetch(ctx, path, q)
		if path == "/v1/status" {
			checks++
			if checks == 2 {
				value.(map[string]any)["boundary"] = float64(43)
			}
		}
		if path == "/v1/sessions" {
			value.(map[string]any)["boundary"] = float64(43)
		}
		return value, err
	}
	rows := transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, advanced, initMessage, initializedMessage, call(2, "sessions.list", `{}`))
	payload := structured(t, rows[1])
	if payload["provenance"].(map[string]any)["archive_boundary"] != float64(43) {
		t.Fatalf("response boundary was taken from preceding status: %v", payload)
	}
	checks = 0
	stale := func(ctx context.Context, path string, q url.Values) (any, error) {
		value, err := baseFetch(ctx, path, q)
		if path == "/v1/sessions" {
			value.(map[string]any)["boundary"] = float64(43)
		}
		return value, err
	}
	rows = transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, stale, initMessage, initializedMessage, call(2, "sessions.list", `{}`))
	payload = structured(t, rows[1])
	if payload["error"] != "archive_boundary_inconsistent" || payload["provenance"].(map[string]any)["archive_boundary"] != "unknown" {
		t.Fatalf("inconsistent boundary accepted: %v", payload)
	}
}

func TestTranscriptWhitespaceInstructionRoles(t *testing.T) {
	page := map[string]any{"items": []any{
		map[string]any{"role": "system ", "kind": "message", "text": "hidden"},
		map[string]any{"role": " developer", "kind": "native_note", "text": "also hidden"},
		map[string]any{"role": "assistant", "kind": "native_note", "text": "visible"},
	}}
	filterTranscript(page)
	items := page["items"].([]any)
	if len(items) != 1 || page["omitted_instruction_blocks"] != 2 || items[0].(map[string]any)["evidence_class"] != "native_note" {
		t.Fatalf("instruction roles escaped filtering: %v", page)
	}
}

func TestTailFeedDoesNotClaimSnapshotHighWaterAsBoundary(t *testing.T) {
	baseFetch, _ := testFetch(t)
	feed := func(ctx context.Context, path string, q url.Values) (any, error) {
		value, err := baseFetch(ctx, path, q)
		if path == "/v1/status" {
			value.(map[string]any)["boundary"] = float64(50)
		}
		if path == "/v1/changes" {
			value = map[string]any{
				"phase": "tail", "high_water": float64(42),
				"events": []any{map[string]any{
					"sequence": float64(45), "namespace": "fixture:one",
					"source_revision": "sha256:" + strings.Repeat("f", 64),
				}},
			}
		}
		return value, err
	}
	rows := transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, feed, initMessage, initializedMessage, call(2, "changes.read", `{}`))
	payload := structured(t, rows[1])
	if payload["provenance"].(map[string]any)["archive_boundary"] != "unknown" {
		t.Fatalf("tail feed claimed snapshot cutoff as read boundary: %v", payload)
	}
	event := payload["data"].(map[string]any)["events"].([]any)[0].(map[string]any)
	if event["sequence"] != float64(45) || event["provenance"].(map[string]any)["source_revision"] != "sha256:"+strings.Repeat("f", 64) {
		t.Fatalf("tail event lost exact revision: %v", event)
	}
}

func TestExactReadErrorSeparatesRequestedAndConfirmedRevision(t *testing.T) {
	revision := "sha256:" + strings.Repeat("a", 64)
	args := fmt.Sprintf(`{"record":"r1","revision":%q,"adapter":"v1"}`, revision)
	for _, code := range []string{"not_found", "evidence_redacted"} {
		t.Run(code, func(t *testing.T) {
			baseFetch, _ := testFetch(t)
			fetch := func(ctx context.Context, path string, q url.Values) (any, error) {
				if path == "/v1/record" {
					return nil, errors.New(code)
				}
				return baseFetch(ctx, path, q)
			}
			rows := transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, fetch, initMessage, initializedMessage, call(2, "artifacts.get", args))
			payload := structured(t, rows[1])
			prov := payload["provenance"].(map[string]any)
			if payload["error"] != code || prov["target_revision"] != revision || prov["source_revision"] != "unknown" {
				t.Fatalf("error incorrectly confirmed source revision: %v", payload)
			}
		})
	}
	baseFetch, _ := testFetch(t)
	mismatch := func(ctx context.Context, path string, q url.Values) (any, error) {
		value, err := baseFetch(ctx, path, q)
		if path == "/v1/record" {
			value.(map[string]any)["record"].(map[string]any)["adapter_version"] = "wrong"
		}
		return value, err
	}
	rows := transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, mismatch, initMessage, initializedMessage, call(2, "artifacts.get", args))
	payload := structured(t, rows[1])
	prov := payload["provenance"].(map[string]any)
	if payload["error"] != "exact_revision_mismatch" || prov["target_revision"] != revision || prov["source_revision"] != "unknown" {
		t.Fatalf("mismatch incorrectly confirmed source revision: %v", payload)
	}
}
