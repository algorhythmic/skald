package daemon

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/algorhythmic/skald/internal/observability"
)

// Administrative diagnostics are separate from namespace-scoped evidence APIs
// and are never exposed by the MCP adapter. The private listener checks UID.
func (a *API) diagnostics(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/manage/metrics" && r.URL.Path != "/manage/profile" {
		return false
	}
	fail := func(status int, code string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
	}
	if r.Method != "GET" || r.ContentLength > 0 {
		fail(http.StatusBadRequest, "invalid_request")
		return true
	}
	q := r.URL.Query()
	if r.URL.Path == "/manage/metrics" {
		if len(q) != 0 {
			fail(http.StatusBadRequest, "invalid_request")
			return true
		}
		_ = json.NewEncoder(w).Encode(a.Collector.Metrics.Snapshot())
		return true
	}
	if !a.Config.Telemetry.EnableProfiling {
		fail(http.StatusForbidden, "profiling_disabled")
		return true
	}
	for k, v := range q {
		if (k != "kind" && k != "seconds") || len(v) != 1 {
			fail(http.StatusBadRequest, "invalid_request")
			return true
		}
	}
	seconds := 0
	var err error
	if raw := q.Get("seconds"); raw != "" {
		seconds, err = strconv.Atoi(raw)
		if err != nil {
			fail(http.StatusBadRequest, "invalid_profile_duration")
			return true
		}
	}
	profile, err := observability.Profile(r.Context(), q.Get("kind"), seconds)
	if err != nil {
		switch err.Error() {
		case "invalid_profile_duration", "invalid_profile_kind", "profile_in_progress", "profile_too_large":
			fail(http.StatusBadRequest, err.Error())
		default:
			fail(http.StatusServiceUnavailable, "profile_unavailable")
		}
		return true
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(profile)
	return true
}
