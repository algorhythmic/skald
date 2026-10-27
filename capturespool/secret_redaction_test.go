package capturespool

import (
	"encoding/json"
	"github.com/algorhythmic/skald/sessioncapture"
	"testing"
)

func TestSensitiveEnvelopeNotSpooled(t *testing.T) {
	d := fixture(t)
	d.Batch.Records[0].Record.Extensions = map[string]json.RawMessage{"password": json.RawMessage(`"synthetic"`)}
	d, e := sessioncapture.NewDelivery(d.Source, d.Previous, d.Batch)
	if e != nil {
		t.Fatal("fixture invalid")
	}
	s := open(t, t.TempDir(), []string{"archive"}, generous())
	defer s.Close()
	if e = s.Enqueue(d, []string{"archive"}); e == nil || e.Error() != "secret_redaction_required" {
		t.Fatal("sensitive envelope escaped spool gate")
	}
}
