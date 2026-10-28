package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestQuarantineContinuesCaptureAndStatusAlert(t *testing.T) {
	c, file := testCollector(t)
	ctx := context.Background()
	c.Scan(ctx)
	hidden := strings.Repeat("Q", 18)
	bad := `{"type":"user","sessionId":"synthetic-claude","message":{"role":"user","content":"` + hidden + `"},"nested":` + strings.Repeat("[", 140) + `0` + strings.Repeat("]", 140) + "}\n"
	good := `{"type":"assistant","sessionId":"synthetic-claude","uuid":"after-quarantine","message":{"role":"assistant","content":"ordinary"}}` + "\n"
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("fixture append")
	}
	_, err = f.WriteString(bad + good)
	f.Close()
	if err != nil {
		t.Fatal("fixture write")
	}
	c.Scan(ctx)
	stats, err := c.Store.Stats(ctx, c.Config.Namespaces())
	if err != nil || stats["record_versions"] != int64(10) {
		t.Fatal("quarantine prevented later ingestion")
	}
	api := &API{Store: c.Store, Collector: c, Config: c.Config}
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest("GET", "/v1/status?namespace="+url.QueryEscape(c.Config.Sources[0].Namespace), nil))
	var status map[string]json.RawMessage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil {
		t.Fatal("status unavailable")
	}
	var q struct {
		Total    int64  `json:"total"`
		LastHour int64  `json:"last_hour"`
		Health   string `json:"health"`
		Reason   string `json:"reason"`
	}
	if json.Unmarshal(status["quarantine"], &q) != nil || q.Total != 1 || q.LastHour != 1 || q.Health != "degraded" || q.Reason != "redaction_quarantined" || strings.Contains(w.Body.String(), hidden) {
		t.Fatal("status quarantine alert missing or leaked")
	}
}
