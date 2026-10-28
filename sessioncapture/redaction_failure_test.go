package sessioncapture

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/algorhythmic/skald/internal/secretredact"
)

func attributionFixture(t *testing.T) Captured {
	t.Helper()
	s := Source{Namespace: "fixture:attribution", Provider: Claude, StreamID: "one", ConversationID: "synthetic"}
	raw := []byte(`{"type":"user","sessionId":"synthetic","message":{"role":"user","content":"ordinary"}}` + "\n")
	batch, err := Read(bytes.NewReader(raw), s, Checkpoint{}, DefaultLimits(), time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	if err != nil || len(batch.Records) != 1 {
		t.Fatal("fixture capture")
	}
	return batch.Records[0]
}

func TestResidualFailureAttributesQuarantineRule(t *testing.T) {
	c := attributionFixture(t)
	residual := []byte("password=\"" + strings.Repeat("Q", 18) + "\"")
	failure := residualFailure(residual)
	if !errors.Is(failure, secretredact.ErrSensitive) || failure.Error() != secretredact.ErrSensitive.Error() {
		t.Fatal("unstable failure code")
	}
	q := QuarantineRecord(c, 0, failure)
	meta, ok := q.QuarantineMetadata()
	if !ok || meta.PatternID != "password_assignment" || meta.PatternCount != 0 || q.Verify() != nil || len(q.Redaction.Rules) != 1 || q.Redaction.Rules[0] != "password_assignment" || q.Redaction.Counts["password_assignment"] != 1 {
		t.Fatal("known residual rule not attributed")
	}
	wire, _ := json.Marshal(q)
	if bytes.Contains(wire, residual) || bytes.Contains(wire, []byte(strings.Repeat("Q", 18))) {
		t.Fatal("attribution retained content")
	}
}

func TestMultipleResidualRulesSortedAndCounted(t *testing.T) {
	c := attributionFixture(t)
	residual := []byte("password=\"" + strings.Repeat("Q", 18) + "\"\n" + "gh" + "p_" + strings.Repeat("A", 36))
	q := QuarantineRecord(c, 0, residualFailure(residual, residual))
	meta, ok := q.QuarantineMetadata()
	if !ok || meta.PatternID != "github_token" || meta.PatternCount != 2 || q.Verify() != nil || len(q.Redaction.Rules) != 2 || q.Redaction.Rules[0] != "github_token" || q.Redaction.Rules[1] != "password_assignment" || q.Redaction.Counts["github_token"] != 2 || q.Redaction.Counts["password_assignment"] != 2 {
		t.Fatal("multiple rule attribution invalid")
	}
	q.Record.Extensions["skald_quarantine"] = json.RawMessage(`{"ordinal":0,"pattern_id":"unknown-policy","reason":"redaction_quarantined","source_offset":0}`)
	if q.Verify() == nil {
		t.Fatal("unknown attribution accepted")
	}
}

func TestUnattributableFailureRetainsGenericQuarantine(t *testing.T) {
	c := attributionFixture(t)
	failures := []error{errors.New("synthetic detector failure detail"), residualFailure([]byte(strings.Repeat("[", 140) + "0" + strings.Repeat("]", 140)))}
	for _, failure := range failures {
		q := QuarantineRecord(c, 0, failure)
		meta, ok := q.QuarantineMetadata()
		if !ok || meta.PatternID != QuarantinePattern || meta.PatternCount != 0 || q.Verify() != nil || q.Redaction.Counts[QuarantinePattern] != 1 {
			t.Fatal("generic detector failure invalid")
		}
		wire, _ := json.Marshal(q)
		if bytes.Contains(wire, []byte("synthetic detector failure detail")) {
			t.Fatal("error text retained")
		}
	}
}
