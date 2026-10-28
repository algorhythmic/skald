package mcp

import (
	"context"
	"encoding/json"
	"github.com/algorhythmic/skald/internal/testsupport"
	"net/url"
	"strings"
	"testing"
)

func TestSensitiveMCPTranscriptSanitized(t *testing.T) {
	base, _ := testFetch(t)
	fetch := func(ctx context.Context, path string, q url.Values) (any, error) {
		value, err := base(ctx, path, q)
		if err == nil && path == "/v1/transcript" {
			for _, item := range value.(map[string]any)["items"].([]any) {
				item.(map[string]any)["text"] = "password=\"" + strings.Repeat("S", 18) + "\""
			}
		}
		return value, err
	}
	rows := transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, fetch, initMessage, initializedMessage, call(2, "transcript.read", `{"conversation":"conversation-1"}`))
	if len(rows) != 2 {
		t.Fatal("unexpected reply count")
	}
	wire, _ := json.Marshal(rows[1])
	if rows[1]["error"] != nil || strings.Contains(string(wire), strings.Repeat("S", 18)) || !strings.Contains(string(wire), "[REDACTED:") {
		t.Fatal("MCP did not return a sanitized reply")
	}
}
func TestEveryPatternMCPProjectionSanitized(t *testing.T) {
	for _, fixture := range testsupport.Secrets() {
		t.Run(fixture.Rule, func(t *testing.T) {
			base, _ := testFetch(t)
			fetch := func(ctx context.Context, path string, q url.Values) (any, error) {
				v, e := base(ctx, path, q)
				if e == nil && path == "/v1/transcript" {
					for _, item := range v.(map[string]any)["items"].([]any) {
						item.(map[string]any)["text"] = fixture.Input
					}
				}
				return v, e
			}
			rows := transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, fetch, initMessage, initializedMessage, call(2, "transcript.read", `{"conversation":"conversation-1"}`))
			wire, _ := json.Marshal(rows)
			if len(rows) != 2 || rows[1]["error"] != nil || strings.Contains(string(wire), fixture.Value) || !strings.Contains(string(wire), "[REDACTED:") {
				t.Fatal("MCP secret projection not sanitized")
			}
		})
	}
}

func TestMCPQuarantineStatusProjection(t *testing.T) {
	base, _ := testFetch(t)
	fetch := func(ctx context.Context, path string, q url.Values) (any, error) {
		value, err := base(ctx, path, q)
		if err == nil && path == "/v1/status" {
			value.(map[string]any)["quarantine"] = map[string]any{"total": 1, "last_hour": 1, "health": "degraded", "reason": "redaction_quarantined"}
		}
		return value, err
	}
	rows := transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, fetch, initMessage, initializedMessage, call(2, "status", `{}`))
	wire, _ := json.Marshal(rows)
	if !strings.Contains(string(wire), "redaction_quarantined") || !strings.Contains(string(wire), "degraded") {
		t.Fatal("MCP quarantine alert omitted")
	}
}
