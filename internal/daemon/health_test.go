package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/internal/archive"
)

func TestSourceHealthLossRestartAndReappearance(t *testing.T) {
	c, file := testCollector(t)
	ctx := context.Background()
	r := c.Config.Sources[0]
	health := func(collector *Collector) archive.SourceHealth {
		t.Helper()
		items, err := collector.Store.Health(ctx, []string{r.Namespace})
		if err != nil || len(items) != 1 {
			t.Fatal(items, err)
		}
		return items[0]
	}
	c.Scan(ctx)
	before := health(c)
	if before.Health != "available" || !before.Enrolled || !before.CheckpointAvailable || before.LastCapturedAt == nil {
		t.Fatal("initial source capture was not visible", before)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	c.Scan(ctx)
	lost := health(c)
	if lost.Health != "unavailable" || c.Issues()[r.Key()] != "source_unavailable" || !lost.Enrolled || lost.CapturedOffset != before.CapturedOffset || !reflect.DeepEqual(lost.LastCapturedAt, before.LastCapturedAt) {
		t.Fatal("source loss hid enrollment or changed capture evidence", lost, c.Issues())
	}
	restarted := &Collector{Store: c.Store, Config: c.Config}
	if err := restarted.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if h := health(restarted); h.Health != "unknown" || !h.CheckpointAvailable || h.CapturedOffset != before.CapturedOffset {
		t.Fatal("restart invented availability or lost checkpoint", h)
	}
	restarted.Scan(ctx)
	if h := health(restarted); h.Health != "unavailable" {
		t.Fatal("restart concealed missing source", h)
	}
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	restarted.Scan(ctx)
	after := health(restarted)
	if after.Health != "available" || len(restarted.Issues()) != 0 || after.CapturedOffset != before.CapturedOffset || !reflect.DeepEqual(after.Generation, before.Generation) || !reflect.DeepEqual(after.NextOrdinal, before.NextOrdinal) || !reflect.DeepEqual(after.LastCapturedAt, before.LastCapturedAt) {
		t.Fatal("source reappearance duplicated or refreshed old evidence", before, after, restarted.Issues())
	}
}

func TestStatusHealthMetadataAndIssuesStayScoped(t *testing.T) {
	c, file := testCollector(t)
	ctx := context.Background()
	r := c.Config.Sources[0]
	c.Scan(ctx)
	foreign := r
	foreign.Namespace, foreign.StreamID = "fixture:other", "other"
	if err := c.Store.Enroll(ctx, []archive.Registration{foreign}); err != nil {
		t.Fatal(err)
	}
	c.Config.Sources = append(c.Config.Sources, foreign)
	c.issue(foreign.Key(), "source_unavailable")
	api := &API{Store: c.Store, Collector: c, Config: c.Config}
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest("GET", "/v1/status?namespace="+url.QueryEscape(r.Namespace), nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var status struct {
		Sources []archive.SourceHealth `json:"sources"`
		Total   int64                  `json:"sources_total"`
		Omitted int64                  `json:"sources_omitted"`
		Issues  map[string]string      `json:"capture_issues"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Total != 1 || status.Omitted != 0 || len(status.Sources) != 1 || len(status.Issues) != 0 || status.Sources[0].Namespace != r.Namespace || !status.Sources[0].CheckpointAvailable {
		t.Fatal("status leaked another namespace or lost checkpoint", w.Body.String())
	}
	for _, private := range []string{file, r.Root, foreign.Namespace, foreign.Key(), "Keep archive history", `"raw":`, `"body":`} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("status exposed private metadata/content %q", private)
		}
	}
	w = httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest("GET", "/v1/status?namespace=forbidden", nil))
	if w.Code != 403 || strings.TrimSpace(w.Body.String()) != `{"error":"scope_denied"}` {
		t.Fatal("status scope denial failed", w.Code, w.Body.String())
	}
}
