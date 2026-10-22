package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestMCPRequiresExplicitNamespaceAndSocket(t *testing.T) {
	for _, args := range [][]string{
		{"--socket", "/tmp/skald.sock"},
		{"--namespace", "fixture:one"},
		{"--socket", "relative.sock", "--namespace", "fixture:one"},
		{"--socket", "/tmp/skald.sock", "--namespace", "fixture:one", "--namespace", "fixture:one"},
		{"--socket", "/tmp/skald.sock", "--namespace", "bad\nnamespace"},
	} {
		var out, stderr bytes.Buffer
		if err := runMCP(args, strings.NewReader(""), &out, &stderr); err == nil {
			t.Fatalf("accepted missing/invalid grant: %v", args)
		}
		if out.Len() != 0 {
			t.Fatalf("wrote non-protocol stdout: %q", out.String())
		}
	}
}

func TestMCPCLIStdioNegotiation(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}` + "\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	var out, stderr bytes.Buffer
	if err := runMCP([]string{"--socket", "/tmp/skald-unopened.sock", "--namespace", "fixture:one"}, strings.NewReader(input), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 2 || !strings.Contains(out.String(), `"protocolVersion":"2025-11-25"`) || !strings.Contains(out.String(), `"name":"transcript.read"`) {
		t.Fatalf("unexpected stdio output: %s", out.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}
