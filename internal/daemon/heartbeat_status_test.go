package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/internal/observability"
)

func TestHeartbeatFailureStatusScopePrecedenceAndRetry(t *testing.T) {
	c, _ := testCollector(t)
	ctx := context.Background()
	first := c.Config.Sources[0]
	foreign, blocked, unavailable, unaffected := first, first, first, first
	foreign.Namespace, foreign.StreamID = "fixture:heartbeat-foreign", "foreign"
	blocked.StreamID, unavailable.StreamID, unaffected.StreamID = "blocked", "unavailable", "unaffected"
	c.Config.Sources = []archive.Registration{first, foreign, blocked, unavailable, unaffected}
	dir := filepath.Join(t.TempDir(), "archive")
	store, err := archive.Open(dir, c.Config.Options())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	c.Store, c.Metrics = store, observability.New()
	if err := store.Register(ctx, c.Config.Sources); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, source := range c.Config.Sources {
		health := "available"
		if source.Key() == blocked.Key() {
			health = "blocked"
		} else if source.Key() == unavailable.Key() {
			health = "unavailable"
		}
		at := now.Add(-2 * time.Minute)
		if i == len(c.Config.Sources)-1 {
			at = now // This source has no due heartbeat and must get no error.
		}
		if err := store.SetHealth(ctx, source, health, at); err != nil {
			t.Fatal(err)
		}
		if err := store.ObserveHealth(ctx, source, health, now); err != nil {
			t.Fatal(err)
		}
	}
	c.issue(blocked.Key(), "capture_blocked")
	c.issue(unavailable.Key(), "source_unavailable")
	db, err := sql.Open("sqlite", archive.DSN(filepath.Join(dir, "archive.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER fail_heartbeat BEFORE UPDATE OF checked_at ON stream_locators BEGIN SELECT RAISE(ABORT,'private SQLite failure detail'); END`); err != nil {
		t.Fatal(err)
	}
	// Cancellation leaves the source observations above intact and runs only
	// the deferred flush, so capture errors remain available to test precedence.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	c.Scan(canceled)
	api := &API{Store: store, Collector: c, Config: c.Config}
	statusIssues := func(namespace string) (map[string]string, string) {
		t.Helper()
		w := httptest.NewRecorder()
		api.ServeHTTP(w, httptest.NewRequest("GET", "/v1/status?namespace="+url.QueryEscape(namespace), nil))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var status struct {
			Issues map[string]string `json:"capture_issues"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		return status.Issues, w.Body.String()
	}
	want := map[string]string{first.Key(): "archive_write_failed", blocked.Key(): "capture_blocked", unavailable.Key(): "source_unavailable"}
	if got, body := statusIssues(first.Namespace); !reflect.DeepEqual(got, want) || strings.Contains(body, foreign.Key()) || strings.Contains(body, "private SQLite") || strings.Contains(body, "health_heartbeat") {
		t.Fatal("heartbeat status lost affected source, precedence, or namespace isolation", got, body)
	}
	if got, body := statusIssues(foreign.Namespace); !reflect.DeepEqual(got, map[string]string{foreign.Key(): "archive_write_failed"}) || strings.Contains(body, first.Key()) {
		t.Fatal("heartbeat failure did not follow source namespace", got, body)
	}
	// Removing a capture issue reveals the still unresolved heartbeat failure.
	c.issue(blocked.Key(), "")
	if got := c.Issues()[blocked.Key()]; got != "archive_write_failed" {
		t.Fatal("capture issue clearing also erased unresolved heartbeat", got)
	}
	c.issue(blocked.Key(), "capture_blocked")
	if _, err := db.Exec("DROP TRIGGER fail_heartbeat"); err != nil {
		t.Fatal(err)
	}
	c.Scan(canceled)
	want = map[string]string{blocked.Key(): "capture_blocked", unavailable.Key(): "source_unavailable"}
	if got, _ := statusIssues(first.Namespace); !reflect.DeepEqual(got, want) {
		t.Fatal("retry did not clear heartbeat errors or erased capture errors", got)
	}
	if got, _ := statusIssues(foreign.Namespace); len(got) != 0 {
		t.Fatal("retry left a foreign heartbeat error", got)
	}
	counters := c.Metrics.Snapshot().Counters
	if counters["health_flush_errors"] != 1 || counters["health_heartbeat_transactions"] != 1 || counters["health_heartbeat_sources"] != 4 {
		t.Fatal("heartbeat retry metrics do not match scoped failures", counters)
	}
}
