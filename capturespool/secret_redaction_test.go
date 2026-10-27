package capturespool

import (
	"encoding/json"
	"github.com/algorhythmic/skald/sessioncapture"
	"testing"
)

func TestSensitiveEnvelopeSpooledSanitized(t *testing.T) {
	d := fixture(t)
	d.Batch.Records[0].Record.Extensions = map[string]json.RawMessage{"password": json.RawMessage(`"synthetic"`)}
	d, e := sessioncapture.NewDelivery(d.Source, d.Previous, d.Batch)
	if e != nil {
		t.Fatal("fixture invalid")
	}
	s := open(t, t.TempDir(), []string{"archive"}, generous())
	defer s.Close()
	if e = s.Enqueue(d, []string{"archive"}); e != nil {
		t.Fatal("sanitized envelope not spooled")
	}
	if d.Version != 2 || d.Batch.Records[0].Redaction == nil {
		t.Fatal("sanitized representation missing")
	}
}
