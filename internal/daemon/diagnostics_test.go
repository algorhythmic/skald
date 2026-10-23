package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/internal/observability"
)

func TestPrivateDiagnosticsBoundsAndProfilingOptIn(t *testing.T) {
	a := &API{Collector: &Collector{Metrics: observability.New()}}
	a.Collector.Metrics.Add("source_checks", 615)
	for _, tc := range []struct {
		method, path string
		want         int
	}{{"GET", "/manage/metrics", 200}, {"GET", "/manage/metrics?namespace=ungranted", 400}, {"POST", "/manage/metrics", 400}, {"GET", "/manage/profile?kind=cpu&seconds=1", 403}} {
		response := httptest.NewRecorder()
		a.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
		if response.Code != tc.want {
			t.Fatal(tc, response.Code, response.Body.String())
		}
		if tc.path == "/manage/metrics" && tc.want == 200 {
			var s observability.Snapshot
			if err := json.Unmarshal(response.Body.Bytes(), &s); err != nil || s.Counters["source_checks"] != 615 {
				t.Fatal(s, err)
			}
		}
	}
	a.Config.Telemetry.EnableProfiling = true
	for _, path := range []string{"/manage/profile?kind=cpu&seconds=31", "/manage/profile?kind=heap&namespace=private", "/manage/profile?kind=heap&kind=cpu"} {
		response := httptest.NewRecorder()
		a.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 400 {
			t.Fatal(path, response.Code)
		}
	}
	response := httptest.NewRecorder()
	a.ServeHTTP(response, httptest.NewRequest("GET", "/manage/profile?kind=heap&seconds=0", nil))
	if response.Code != 200 || response.Header().Get("Content-Type") != "application/octet-stream" || strings.HasPrefix(response.Body.String(), "{") {
		t.Fatal("heap profile response", response.Code)
	}
}
