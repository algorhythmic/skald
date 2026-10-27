package daemon

import (
	"encoding/json"
	"github.com/algorhythmic/skald/internal/testsupport"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSensitiveResponseSanitized(t *testing.T) {
	for _, value := range []any{map[string]string{"password": "synthetic"}, map[string][]byte{"raw": []byte(`{"password":"synthetic"}`)}} {
		w := httptest.NewRecorder()
		respond(w, value, nil)
		var v map[string]string
		if json.Unmarshal(w.Body.Bytes(), &v) != nil || strings.Contains(w.Body.String(), "synthetic") || w.Code != 200 {
			t.Fatal("sensitive response escaped")
		}
	}
}
func TestEveryPatternHTTPProjectionSanitized(t *testing.T) {
	for _, fixture := range testsupport.Secrets() {
		t.Run(fixture.Rule, func(t *testing.T) {
			w := httptest.NewRecorder()
			respond(w, map[string]string{"text": fixture.Input}, nil)
			if w.Code != 200 || strings.Contains(w.Body.String(), fixture.Value) || !strings.Contains(w.Body.String(), "[REDACTED:") {
				t.Fatal("HTTP secret projection not sanitized")
			}
		})
	}
}
