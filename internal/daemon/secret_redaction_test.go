package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestSensitiveResponseWithheld(t *testing.T) {
	for _, value := range []any{map[string]string{"password": "synthetic"}, map[string][]byte{"raw": []byte(`{"password":"synthetic"}`)}} {
		w := httptest.NewRecorder()
		respond(w, value, nil)
		var v map[string]string
		if json.Unmarshal(w.Body.Bytes(), &v) != nil || v["error"] != "secret_redaction_required" || w.Code != 422 {
			t.Fatal("sensitive response escaped")
		}
	}
}
