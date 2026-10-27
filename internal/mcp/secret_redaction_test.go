package mcp

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

func TestSensitiveMCPTranscriptWithheld(t *testing.T) {
	base, _ := testFetch(t)
	fetch := func(ctx context.Context, path string, q url.Values) (any, error) {
		value, err := base(ctx, path, q)
		if err == nil && path == "/v1/transcript" {
			for _, item := range value.(map[string]any)["items"].([]any) {
				item.(map[string]any)["text"] = "password=" + strings.Repeat("S", 18)
			}
		}
		return value, err
	}
	rows := transact(t, Config{Socket: "/tmp/test.sock", Namespaces: []string{"fixture:one"}}, fetch, initMessage, initializedMessage, call(2, "transcript.read", `{"conversation":"conversation-1"}`))
	if len(rows) != 2 {
		t.Fatal("unexpected reply count")
	}
	e, ok := rows[1]["error"].(map[string]any)
	if !ok || e["message"] != "secret_redaction_required" {
		t.Fatal("sensitive MCP reply escaped")
	}
}
