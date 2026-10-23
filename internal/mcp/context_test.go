package mcp

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestRankedContextScopeBudgetAndStrictArguments(t *testing.T) {
	base, _ := testFetch(t)
	foreign := false
	fetch := func(ctx context.Context, path string, q url.Values) (any, error) {
		if path != "/v1/context" && path != "/v1/descriptions" {
			return base(ctx, path, q)
		}
		if q.Get("namespace") != "fixture:one" {
			t.Fatal("scope missing", q)
		}
		if path == "/v1/descriptions" {
			return map[string]any{"boundary": float64(42), "kind": "unavailable", "prior_recaps": []any{}}, nil
		}
		if q.Get("text") != "connection" || q.Get("max_bytes") != "1024" {
			t.Fatal("query arguments lost", q)
		}
		ns := "fixture:one"
		if foreign {
			ns = "other"
		}
		items := []any{map[string]any{"id": "r:1", "text": "source evidence", "namespace": ns, "role": "assistant", "adapter_version": "v1", "ref": map[string]any{"record_key": "r1", "source_revision": "sha256:" + strings.Repeat("a", 64)}}}
		b, _ := json.Marshal(items)
		return map[string]any{"boundary": float64(42), "status": "ok", "context": items, "context_bytes": float64(len(b)), "coverage": "complete"}, nil
	}
	cfg := Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}
	rows := transact(t, cfg, fetch, initMessage, initializedMessage, call(2, "context.query", `{"text":"connection","max_bytes":1024}`), call(3, "descriptions.read", `{"conversation":"c"}`))
	data := structured(t, rows[1])["data"].(map[string]any)
	encoded, _ := json.Marshal(data["context"])
	if int(data["context_bytes"].(float64)) != len(encoded) {
		t.Fatal("MCP changed byte-bounded context", data)
	}
	foreign = true
	rows = transact(t, cfg, fetch, initMessage, initializedMessage, call(2, "context.query", `{"text":"connection","max_bytes":1024}`))
	if !rows[1]["result"].(map[string]any)["isError"].(bool) {
		t.Fatal("accepted foreign namespace", rows)
	}
	for _, args := range []string{`{}`, `{"text":"x","max_bytes":0}`, `{"text":"x","max_bytes":65537}`, `{"text":"x","raw":true}`, `{"text":"x","max_bytes":null}`} {
		rows = transact(t, cfg, base, initMessage, initializedMessage, call(2, "context.query", args))
		if !rows[1]["result"].(map[string]any)["isError"].(bool) {
			t.Fatal("invalid query accepted", args, rows)
		}
	}
}
