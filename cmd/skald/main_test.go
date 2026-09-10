package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
)

func TestInspectAndResumeCLI(t *testing.T) {
	args := []string{"inspect", "--provider", "claude_code", "--namespace", "fixture:claude", "--stream-id", "fixture", "--file", "../../testdata/claude/session.jsonl", "--limit", "1", "--raw"}
	var out, stderr bytes.Buffer
	if err := run(args, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var batch sessioncapture.Batch
	if err := json.Unmarshal(out.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Records) != 1 || len(batch.Records[0].Raw) == 0 || !batch.More {
		t.Fatal("incorrect first batch")
	}
	cp, _ := json.Marshal(batch.Checkpoint)
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	if err := os.WriteFile(path, cp, 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(append(args, "--checkpoint", path), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var resumed sessioncapture.Batch
	_ = json.Unmarshal(out.Bytes(), &resumed)
	if resumed.Records[0].Record.RecordKey == batch.Records[0].Record.RecordKey {
		t.Fatal("checkpoint did not advance read")
	}
}

func TestCLIRejectsBadArgumentsAndPartialIdentity(t *testing.T) {
	for _, args := range [][]string{{"discover"}, {"probe"}, {"validate", "--file", "missing"}, {"version", "ignored"}, {"unknown"}, {"probe", "--provider", "codex", "--raw"}} {
		var out, errout bytes.Buffer
		if err := run(args, &out, &errout); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	path := filepath.Join(t.TempDir(), "unbound.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"future\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	err := run([]string{"inspect", "--provider", "codex", "--namespace", "fixture", "--stream-id", "fixture", "--file", path}, &out, &errout)
	if err == nil || !strings.Contains(out.String(), "conversation_identity_unavailable") {
		t.Fatal("blocked capture not reported")
	}
}

func TestJSONEscapesTerminalControlsWithoutChangingData(t *testing.T) {
	want := map[string]string{"text": "hello\x1b[31m\u009b\u202e world\U000e0001"}
	var out bytes.Buffer
	if err := writeJSON(&out, want); err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{"\x1b", "\u009b", "\u202e", "\U000e0001"} {
		if strings.Contains(out.String(), unsafe) {
			t.Fatal("literal terminal control emitted")
		}
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["text"] != want["text"] {
		t.Fatal("escaping changed archived text")
	}
}
