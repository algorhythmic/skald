package sessioncapture_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
	_ "modernc.org/sqlite"
)

// devinFixture creates a minimal acp-messages store: meta with a native title,
// a thought, a message, a tool call with workdir evidence and a plan update.
func devinFixture(t *testing.T, dir string) string {
	t.Helper()
	name := "11111111-2222-3333-4444-555555555555.db"
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		"CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)",
		"CREATE TABLE messages (position INTEGER PRIMARY KEY, kind TEXT NOT NULL, payload TEXT NOT NULL)",
		`INSERT INTO meta VALUES ('schema_version','1'),('info','{"title":"Adjust skald theme"}'),('message_count','4')`,
		`INSERT INTO messages VALUES
		 (0,'agent_thought','{"kind":"agent_thought","content":[{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking about it"}}]}'),
		 (1,'agent_message','{"kind":"agent_message","content":[{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hello "}},{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"there"}}]}'),
		 (2,'tool_call','{"kind":"tool_call","content":{"toolCallId":"exec_0","title":"Ran ls","kind":"execute","rawInput":{"command":"ls -la","workdir":"/home/x/proj"}}}'),
		 (3,'plan','{"kind":"plan","content":{"sessionUpdate":"plan","entries":[{"content":"step one"},{"content":"step two"}]}}')`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return name
}

func TestDevinEmptyStoreRejected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	name := "33333333-4444-5555-6666-777777777777.db"
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)",
		"CREATE TABLE messages (position INTEGER PRIMARY KEY, kind TEXT NOT NULL, payload TEXT NOT NULL)",
		`INSERT INTO meta VALUES ('schema_version','1'),('info','{}'),('message_count','0')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if _, err := sessioncapture.Identify(ctx, dir, name, sessioncapture.Devin); err == nil || err.Error() != "empty_session_store" {
		t.Fatal("metadata-only store must not enroll", err)
	}
}

func TestDevinDumpIdentifyAndNormalize(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	name := devinFixture(t, dir)
	identity, err := sessioncapture.Identify(ctx, dir, name, sessioncapture.Devin)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ConversationID != "11111111-2222-3333-4444-555555555555" || identity.ProviderVersion != "acp-store-1" {
		t.Fatal("devin identity mismatch", identity)
	}
	dump, err := sessioncapture.DevinDump(ctx, dir, name)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(dump, []byte(`{"kind":"meta"`)) || strings.Count(string(dump), "\n") != 5 {
		t.Fatal("canonical dump shape", string(dump))
	}
	s := sessioncapture.Source{Namespace: "fixture:devin", Provider: sessioncapture.Devin, StreamID: "s1", ConversationID: identity.ConversationID}
	batch, err := sessioncapture.Read(bytes.NewReader(dump), s, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Records) != 5 {
		t.Fatalf("expected 5 records, got %d", len(batch.Records))
	}
	title := batch.Records[0].Record
	if title.Kind != "title" || title.Body.Text != "Adjust skald theme" {
		t.Fatal("meta line did not project the native title", title.Kind, title.Body.Text)
	}
	if batch.Records[1].Record.Kind != "native_note" {
		t.Fatal("agent_thought should be a native note", batch.Records[1].Record.Kind)
	}
	msg := batch.Records[2].Record
	if msg.Kind != "message" || *msg.Role != "assistant" || msg.Body.Text != "Hello there" {
		t.Fatal("agent_message chunks did not join", msg.Body.Text)
	}
	tool := batch.Records[3].Record
	if tool.Kind != "tool_call" || tool.Body.Parts[0].Name != "Ran ls" || tool.Body.Parts[0].Text != "ls -la" {
		t.Fatal("tool_call projection", tool.Kind)
	}
	var cwd string
	if err := json.Unmarshal(tool.Extensions["native_cwd"], &cwd); err != nil || cwd != "/home/x/proj" {
		t.Fatal("workdir evidence missing", cwd)
	}
	plan := batch.Records[4].Record
	if plan.Kind != "native_note" || plan.Body.Text != "step one · step two" {
		t.Fatal("plan projection", plan.Body.Text)
	}
	for _, rec := range batch.Records {
		if err := rec.Record.VerifyRaw(rec.Raw); err != nil {
			t.Fatal("raw provenance check failed", err)
		}
	}
	// A row change mid-history invalidates the canonical prefix, not silently merges.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE messages SET payload='{"kind":"agent_message","content":[]}' WHERE position=1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	changed, err := sessioncapture.DevinDump(ctx, dir, name)
	if err != nil {
		t.Fatal(err)
	}
	next, err := sessioncapture.Read(bytes.NewReader(changed), s, batch.Checkpoint, sessioncapture.DefaultLimits(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Gaps) == 0 || next.Gaps[0].Code != "continuity_gap" || next.Checkpoint.Epoch != 1 {
		t.Fatal("row rewrite must surface a continuity gap and new epoch", next.Gaps)
	}
}
