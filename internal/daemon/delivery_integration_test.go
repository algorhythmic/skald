package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/algorhythmic/skald/archivefeed"
	"github.com/algorhythmic/skald/capturespool"
	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/sessioncapture"
)

func feedCall(t *testing.T, api *API, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func feedPage(t *testing.T, w *httptest.ResponseRecorder) archivefeed.Page {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("feed status %d: %s", w.Code, w.Body.String())
	}
	page, err := archivefeed.DecodePage(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestArchiveFeedScopeGenerationAndRedactionHTTP(t *testing.T) {
	ctx := context.Background()
	c, _ := testCollector(t)
	if err := c.Store.SetReadScope(ctx, c.Config.Namespaces()); err != nil {
		t.Fatal(err)
	}
	c.Scan(ctx)
	api := &API{Store: c.Store, Collector: c, Config: c.Config}
	if w := feedCall(t, api, "GET", "/v1/capabilities"); w.Code != 200 || !strings.Contains(w.Body.String(), "archive_feed") {
		t.Fatalf("capabilities: %d %s", w.Code, w.Body.String())
	}
	first := feedPage(t, feedCall(t, api, "GET", "/v1/changes?limit=1"))
	if !first.More || first.NextCursor == "" {
		t.Fatal("first bounded feed page did not continue", first)
	}
	api = &API{Store: c.Store, Collector: c, Config: c.Config}
	if err := c.Store.SetReadScope(ctx, c.Config.Namespaces()); err != nil {
		t.Fatal(err)
	}
	feedPage(t, feedCall(t, api, "GET", "/v1/changes?limit=1&cursor="+url.QueryEscape(first.NextCursor)))

	full := feedPage(t, feedCall(t, api, "GET", "/v1/changes?limit=100"))
	var record *archivefeed.Event
	for i := range full.Events {
		if full.Events[i].Kind == "record" {
			record = &full.Events[i]
			break
		}
	}
	if record == nil || record.Locator == nil {
		t.Fatal("record locator absent", full.Events)
	}
	redact := "/manage/redact?namespace=" + url.QueryEscape(record.Namespace) + "&scope=record&target=" + url.QueryEscape(record.RecordKey)
	if w := feedCall(t, api, "POST", redact); w.Code != 200 {
		t.Fatalf("redaction failed: %d %s", w.Code, w.Body.String())
	}
	full = feedPage(t, feedCall(t, api, "GET", "/v1/changes?limit=100"))
	var priorUnavailable, withdrawal bool
	for _, e := range full.Events {
		if e.RecordKey == record.RecordKey && e.SourceRevision == record.SourceRevision && e.Unavailable == "redacted" && e.Locator == nil {
			priorUnavailable = true
		}
		if e.Kind == "redaction" && e.ScopeKey == record.RecordKey {
			withdrawal = true
		}
	}
	if !priorUnavailable || !withdrawal {
		t.Fatal("redaction did not preserve identity and revoke exact locator", full.Events)
	}
	q := url.Values{"record": {record.RecordKey}, "revision": {record.SourceRevision}, "adapter": {record.Locator.AdapterVersion}}
	if w := feedCall(t, api, "GET", "/v1/record?"+q.Encode()); w.Code == 200 || !strings.Contains(w.Body.String(), "evidence_redacted") {
		t.Fatalf("redacted exact read: %d %s", w.Code, w.Body.String())
	}

	if err := c.Store.SetReadScope(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.SetReadScope(ctx, c.Config.Namespaces()); err != nil {
		t.Fatal(err)
	}
	w := feedCall(t, api, "GET", "/v1/changes?limit=1&cursor="+url.QueryEscape(first.NextCursor))
	if w.Code == 200 || !strings.Contains(w.Body.String(), "cursor_expired") {
		t.Fatalf("revoked/regranted scope reused cursor: %d %s", w.Code, w.Body.String())
	}
}

func TestPendingSpoolDrainsWhenNativeSourceMissing(t *testing.T) {
	ctx := context.Background()
	c, file := testCollector(t)
	spool, err := capturespool.Open(filepath.Join(t.TempDir(), "spool"), []string{"skald:standalone-archive"}, capturespool.Limits{MaxPending: 10, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spool.Close() })
	c.Spool = spool
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	source := c.Config.Sources[0]
	batch, err := sessioncapture.Read(bytes.NewReader(raw), source.Source, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := sessioncapture.NewDelivery(source.Source, sessioncapture.Checkpoint{}, batch)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.Enqueue(delivery, []string{"skald:standalone-archive"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	c.Scan(ctx)
	status, err := spool.Status()
	if err != nil || status.Pending != 0 {
		t.Fatalf("source loss blocked spool drain: %+v %v", status, err)
	}
	stats, err := c.Store.Stats(ctx, c.Config.Namespaces())
	if err != nil || stats["record_versions"] != int64(8) {
		t.Fatalf("queued archive delivery lost: %+v %v", stats, err)
	}
	api := &API{Store: c.Store, Collector: c, Config: c.Config}
	w := feedCall(t, api, "GET", "/v1/status")
	if w.Code != 200 {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["capture_delivery"]; !ok {
		t.Fatal("scoped capture delivery status missing", w.Body.String())
	}
}

func TestCaptureDeliveryStatusIsNamespaceScoped(t *testing.T) {
	ctx := context.Background()
	c, file := testCollector(t)
	second := c.Config.Sources[0]
	second.Namespace, second.StreamID = "fixture:other", "other"
	if err := c.Store.Enroll(ctx, []archive.Registration{second}); err != nil {
		t.Fatal(err)
	}
	c.Config.Sources = append(c.Config.Sources, second)
	spool, err := capturespool.Open(filepath.Join(t.TempDir(), "spool"), []string{localDestination}, capturespool.Limits{MaxPending: 10, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spool.Close() })
	c.Spool = spool
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	source := c.Config.Sources[0]
	batch, err := sessioncapture.Read(bytes.NewReader(raw), source.Source, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := sessioncapture.NewDelivery(source.Source, sessioncapture.Checkpoint{}, batch)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.Enqueue(delivery, []string{localDestination}); err != nil {
		t.Fatal(err)
	}
	api := &API{Store: c.Store, Collector: c, Config: c.Config}
	readPending := func(namespace string) int {
		t.Helper()
		w := feedCall(t, api, "GET", "/v1/status?namespace="+url.QueryEscape(namespace))
		if w.Code != 200 {
			t.Fatalf("status %s: %d %s", namespace, w.Code, w.Body.String())
		}
		var body struct {
			Delivery struct {
				Pending int `json:"pending"`
			} `json:"capture_delivery"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Delivery.Pending
	}
	if got := readPending(second.Namespace); got != 0 {
		t.Fatal("foreign namespace backlog leaked", got)
	}
	if got := readPending(source.Namespace); got != 1 {
		t.Fatal("own pending delivery hidden", got)
	}
}

func TestArchiveFeedCursorSurvivesArchiveRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	raw, err := os.ReadFile("../../testdata/claude/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "session.jsonl"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Version: 1, PollMilliseconds: 100, MaxDatabaseBytes: 64 << 20,
		Sources: []archive.Registration{{Source: sessioncapture.Source{Namespace: "fixture:claude", Provider: sessioncapture.Claude, StreamID: "fixture"}, Root: root, Path: "session.jsonl"}}}
	dir := filepath.Join(t.TempDir(), "archive")
	open := func() *Collector {
		t.Helper()
		store, err := archive.Open(dir, cfg.Options())
		if err != nil {
			t.Fatal(err)
		}
		collector := &Collector{Store: store, Config: cfg}
		if err := collector.Initialize(ctx); err != nil {
			store.Close()
			t.Fatal(err)
		}
		if err := store.SetReadScope(ctx, cfg.Namespaces()); err != nil {
			store.Close()
			t.Fatal(err)
		}
		return collector
	}
	firstCollector := open()
	firstCollector.Scan(ctx)
	firstAPI := &API{Store: firstCollector.Store, Collector: firstCollector, Config: cfg}
	first := feedPage(t, feedCall(t, firstAPI, "GET", "/v1/changes?limit=1"))
	if !first.More {
		t.Fatal("fixture did not produce multiple events")
	}
	if err := firstCollector.Store.Close(); err != nil {
		t.Fatal(err)
	}
	secondCollector := open()
	t.Cleanup(func() { secondCollector.Store.Close() })
	secondAPI := &API{Store: secondCollector.Store, Collector: secondCollector, Config: cfg}
	continued := feedPage(t, feedCall(t, secondAPI, "GET", "/v1/changes?limit=1&cursor="+url.QueryEscape(first.NextCursor)))
	if len(continued.Events) != 1 || continued.Events[0].Sequence <= first.Events[0].Sequence || continued.HighWater != first.HighWater {
		t.Fatal("restart changed snapshot cursor", continued)
	}
}
