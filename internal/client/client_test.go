package client

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTransportErrorsAndTypedReads(t *testing.T) {
	c := New("/tmp/synthetic-skald.sock")
	defer c.Close()
	if c.transport.Proxy != nil || !c.transport.DisableCompression {
		t.Fatal("transport may leave Unix socket")
	}
	for _, tt := range []struct {
		status     int
		body, want string
	}{{200, `{"value":"界"}`, ""}, {403, `{"error":"scope_denied"}`, "scope_denied"}, {409, `{"error":"cursor_expired"}`, "cursor_expired"}, {500, `broken`, "daemon_request_failed"}, {200, `broken`, "invalid_daemon_response"}} {
		c.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "skald" || r.URL.Query().Get("namespace") != "fixture:test" {
				t.Fatal("incorrect request")
			}
			return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(tt.body))}, nil
		})
		var out struct{ Value string }
		err := c.Do(context.Background(), "GET", "/v1/status", url.Values{"namespace": {"fixture:test"}}, &out)
		if tt.want == "" {
			if err != nil || out.Value != "界" {
				t.Fatal(out, err)
			}
		} else if err == nil || err.Error() != tt.want {
			t.Fatal(tt, err)
		}
	}
}
