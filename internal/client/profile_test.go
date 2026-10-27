package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestProfileUsesUnixClientAndRejectsErrors(t *testing.T) {
	c := New("/tmp/synthetic-profile.sock")
	defer c.Close()
	for _, tc := range []struct {
		status           int
		body, kind, want string
	}{{200, "profile-bytes", "application/octet-stream", ""}, {403, `{"error":"profiling_disabled"}`, "application/json", "profiling_disabled"}, {200, `{}`, "application/json", "invalid_profile_response"}} {
		c.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "skald" || r.URL.Path != "/manage/profile" || r.URL.Query().Get("kind") != "cpu" || r.URL.Query().Get("seconds") != "10" {
				t.Fatal("incorrect request", r.URL)
			}
			return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.kind}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		})
		var out bytes.Buffer
		err := c.Profile(context.Background(), "cpu", 10, &out)
		if tc.want == "" {
			if err != nil || out.String() != tc.body {
				t.Fatal(out.String(), err)
			}
		} else if err == nil || err.Error() != tc.want {
			t.Fatal(tc, err)
		}
	}
}
