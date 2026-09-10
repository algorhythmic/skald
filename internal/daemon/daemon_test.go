package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/sessioncapture"
)

func testCollector(t *testing.T) (*Collector, string) {
	t.Helper()
	root := t.TempDir()
	raw, err := os.ReadFile("../../testdata/claude/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Version: 1, PollMilliseconds: 100, MaxDatabaseBytes: 64 << 20, Sources: []archive.Registration{{Source: sessioncapture.Source{Namespace: "fixture:claude", Provider: sessioncapture.Claude, StreamID: "fixture"}, Root: root, Path: "session.jsonl"}}}
	s, err := archive.Open(filepath.Join(t.TempDir(), "archive"), cfg.Options())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Register(context.Background(), cfg.Sources); err != nil {
		t.Fatal(err)
	}
	return &Collector{Store: s, Config: cfg}, file
}

func TestCollectorTailPartialLineLossAndRecovery(t *testing.T) {
	c, file := testCollector(t)
	ctx := context.Background()
	r := c.Config.Sources[0]
	c.Scan(ctx)
	before, err := c.Store.Stats(ctx, c.Config.Namespaces())
	if err != nil || before["record_versions"] != int64(8) {
		t.Fatal("initial capture failed", err, c.Issues())
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"synthetic-claude","message":{"role":"assistant","content":"new tail"}}`
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	c.Scan(ctx)
	partial, _ := c.Store.Stats(ctx, c.Config.Namespaces())
	if partial["record_versions"] != before["record_versions"] {
		t.Fatal("partial tail archived")
	}
	health, _ := c.Store.Health(ctx, c.Config.Namespaces())
	if health[0].Health != "partial_line" {
		t.Fatal("partial line not visible")
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	c.Scan(ctx)
	after, _ := c.Store.Stats(ctx, c.Config.Namespaces())
	if after["record_versions"] != int64(9) {
		t.Fatal("tail missed", c.Issues())
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	c.Scan(ctx)
	lost, _ := c.Store.Stats(ctx, c.Config.Namespaces())
	if lost["record_versions"] != int64(9) || c.Issues()[r.Key()] != "source_unavailable" {
		t.Fatal("source loss destroyed archive or hid error")
	}
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	c.Scan(ctx)
	restored, _ := c.Store.Stats(ctx, c.Config.Namespaces())
	if restored["record_versions"] != int64(9) || len(c.Issues()) != 0 {
		t.Fatal("source restoration duplicated content")
	}
}

func TestRootEscapeAndFileReplacement(t *testing.T) {
	c, file := testCollector(t)
	ctx := context.Background()
	outside := filepath.Join(t.TempDir(), "private.jsonl")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, file); err != nil {
		t.Fatal(err)
	}
	c.Scan(ctx)
	stats, _ := c.Store.Stats(ctx, c.Config.Namespaces())
	if stats["record_versions"] != int64(0) || len(c.Issues()) != 1 {
		t.Fatal("symlink escaped configured root")
	}
}

func TestReadAPIScopesExactReadsAndNoWrites(t *testing.T) {
	c, _ := testCollector(t)
	c.Scan(context.Background())
	api := &API{Store: c.Store, Collector: c, Config: c.Config}
	call := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		return w
	}
	w := call("GET", "/v1/sessions")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var sessions archive.Page[archive.Session]
	if err := json.Unmarshal(w.Body.Bytes(), &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions.Items) != 1 {
		t.Fatal("session list empty")
	}
	w = call("GET", "/v1/artifacts?conversation="+url.QueryEscape(sessions.Items[0].Key))
	var artifacts archive.Page[archive.Artifact]
	if err := json.Unmarshal(w.Body.Bytes(), &artifacts); err != nil {
		t.Fatal(err)
	}
	if len(artifacts.Items) != 8 || artifacts.Items[0].Ordinal != 0 || artifacts.Items[3].Kind != "native_recap" {
		t.Fatal("artifact references lost native order", w.Body.String())
	}
	item := artifacts.Items[0]
	q := url.Values{"record": {item.Key}, "revision": {item.Revision}, "adapter": {item.Adapter}}
	path := "/v1/record?" + q.Encode()
	w = call("GET", path)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"raw":`) {
		t.Fatal("default read exposed raw bytes", w.Body.String())
	}
	if w := call("GET", path+"&raw=true"); w.Code != 403 {
		t.Fatal("raw projection not gated", w.Body.String())
	}
	api.Config.AllowRaw = true
	if w := call("GET", path+"&raw=true"); w.Code != 200 || !strings.Contains(w.Body.String(), `"raw":`) {
		t.Fatal("explicit raw read failed")
	}
	if w := call("GET", path+"&namespace=foreign"); w.Code != 403 {
		t.Fatal("scope widened", w.Body.String())
	}
	if w := call("POST", "/v1/sessions"); w.Code != 405 {
		t.Fatal("read API accepted write")
	}
	for _, path := range []string{"/v1/sessions?limit=999999", "/v1/status?namespace=a&namespace=b", "/v1/status?sql=SELECT", "/v1/record?raw=1"} {
		if w := call("GET", path); w.Code == 200 {
			t.Fatal("invalid request accepted", path)
		}
	}
	if w := call("POST", "/manage/backup?destination=/outside"); w.Code == 200 {
		t.Fatal("unconfigured filesystem backup destination accepted")
	}
}

func TestConfigValidationAndNoImplicitRoots(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	for _, raw := range []string{`{"version":2}`, `{"version":1,"poll_interval_ms":0}`, `{"version":1,"surprise":true}`, `{"version":1} {}`, `{"version":1,"sources":[{"namespace":"n","provider":"codex","stream_id":"s","root":"..","path":"../escape"}]}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("invalid config accepted", raw)
		}
	}
	raw := `{"version":1,"sources":[{"namespace":"n","provider":"codex","stream_id":"s","root":"fixtures","path":"session.jsonl"}]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources[0].Root != filepath.Join(dir, "fixtures") || cfg.AllowRaw {
		t.Fatal("config defaults widened collection")
	}
}

func TestTranscriptAPIProjectionAndValidation(t *testing.T) {
	c, _ := testCollector(t)
	c.Scan(context.Background())
	sessions, err := c.Store.Sessions(context.Background(), c.Config.Namespaces(), "", 25)
	if err != nil {
		t.Fatal(err)
	}
	api := &API{Store: c.Store, Collector: c, Config: c.Config}
	base := "/v1/transcript?conversation=" + url.QueryEscape(sessions.Items[0].Key)
	for _, suffix := range []string{"", "&history=true", "&limit=1"} {
		w := httptest.NewRecorder()
		api.ServeHTTP(w, httptest.NewRequest("GET", base+suffix, nil))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var page archive.Page[archive.TranscriptEntry]
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) == 0 {
			t.Fatal("invalid transcript", err)
		}
		if strings.Contains(w.Body.String(), `"raw"`) {
			t.Fatal("raw bytes leaked")
		}
	}
	for _, suffix := range []string{"&limit=26", "&history=maybe", "&raw=true", "&limit=1&limit=2", "&namespace=forbidden"} {
		w := httptest.NewRecorder()
		api.ServeHTTP(w, httptest.NewRequest("GET", base+suffix, nil))
		if w.Code == 200 {
			t.Fatal("invalid read accepted", suffix)
		}
	}
}
