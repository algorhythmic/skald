package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/algorhythmic/skald/internal/archive"
)

func TestExtendedReadsAndCompressionStayScoped(t *testing.T) {
	c, _ := testCollector(t)
	c.Scan(context.Background())
	a := &API{Store: c.Store, Collector: c, Config: c.Config}
	page, e := c.Store.Sessions(context.Background(), c.Config.Namespaces(), "", 10)
	if e != nil || len(page.Items) != 1 {
		t.Fatal(page, e)
	}
	path := "/v1/descriptions?conversation=" + url.QueryEscape(page.Items[0].Key)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	var before archive.DescriptionSelection
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &before) != nil || before.Selected == nil {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, bad := range []string{path + "&namespace=ungranted", path + "&extra=1", "/v1/context?text=x&max_bytes=1", "/manage/compress?max_rows=0"} {
		method := "GET"
		if bad == "/manage/compress?max_rows=0" {
			method = "POST"
		}
		w = httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest(method, bad, nil))
		if w.Code == 200 {
			t.Fatal("invalid request accepted", bad, w.Body.String())
		}
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("POST", "/manage/compress?max_rows=100&max_input_bytes=67108864", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	var after archive.DescriptionSelection
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &after) != nil || after.Selected == nil || after.Selected.Text != before.Selected.Text || after.Selected.Revision != before.Selected.Revision {
		t.Fatal("compression changed scoped description", w.Body.String())
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/v1/context?text=fixture", nil))
	var disabled struct {
		Status, Coverage string
		Context          []any
	}
	if json.Unmarshal(w.Body.Bytes(), &disabled) != nil || disabled.Status != "unavailable" || disabled.Coverage != "retrieval_disabled" || len(disabled.Context) != 0 {
		t.Fatal(w.Body.String())
	}
}
