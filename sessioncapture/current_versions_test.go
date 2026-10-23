package sessioncapture_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
)

// These are synthetic content and identifiers, derived from a metadata-only
// October 22 inventory: Claude 2.1.296/2.1.303 user string/text blocks,
// assistant text/thinking/tool_use, user tool_result, and system away_summary
// with string content; Codex 0.163.0 response_item input_text/output_text.
// No native conversation bodies were copied. Version labels alone are not the
// compatibility evidence: every observed supported shape is exercised below.
func currentClaudeFixture(version string) []byte {
	rows := []string{
		`{"type":"user","uuid":"current-user","sessionId":"synthetic-current","version":%q,"message":{"role":"user","content":"Synthetic current request."}}`,
		`{"type":"user","uuid":"current-text","sessionId":"synthetic-current","message":{"role":"user","content":[{"type":"text","text":"Synthetic text block."}]}}`,
		`{"type":"assistant","uuid":"current-thinking","sessionId":"synthetic-current","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Synthetic internal note."}]}}`,
		`{"type":"assistant","uuid":"current-assistant","sessionId":"synthetic-current","message":{"role":"assistant","content":[{"type":"text","text":"Synthetic current answer."}]}}`,
		`{"type":"assistant","uuid":"current-call","sessionId":"synthetic-current","message":{"role":"assistant","content":[{"type":"tool_use","id":"synthetic-call","name":"Read","input":{"file_path":"synthetic.txt"}}]}}`,
		`{"type":"user","uuid":"current-result","sessionId":"synthetic-current","message":{"role":"user","content":[{"type":"text","text":"Synthetic result context."},{"type":"tool_result","tool_use_id":"synthetic-call","content":"Synthetic tool result."}]}}`,
		`{"type":"system","subtype":"away_summary","uuid":"current-recap","sessionId":"synthetic-current","content":"Synthetic native persisted recap."}`,
	}
	rows[0] = fmt.Sprintf(rows[0], version)
	var out bytes.Buffer
	for _, row := range rows {
		out.WriteString(row)
		out.WriteByte('\n')
	}
	return out.Bytes()
}
func currentCodexFixture() []byte {
	return []byte(
		`{"type":"session_meta","payload":{"id":"synthetic-current","cli_version":"0.163.0","source":"cli","originator":"codex_cli_rs","forked_from_id":null,"cwd":"/synthetic"}}` + "\n" +
			`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"Synthetic developer context."}]}}` + "\n" +
			`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic current request."}]}}` + "\n" +
			`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Synthetic current answer."}]}}` + "\n")
}

func currentCaptureWithRestarts(t *testing.T, raw []byte, provider, version string) sessioncapture.Batch {
	t.Helper()
	s := source(provider)
	s.ProviderVersion = version
	whole := read(t, raw, s, sessioncapture.Checkpoint{})
	if whole.Blocked || whole.Pending || whole.Checkpoint.HasGaps {
		t.Fatal("current schema capture incomplete", whole.Gaps)
	}
	limits := sessioncapture.DefaultLimits()
	limits.Records = 1
	var cp sessioncapture.Checkpoint
	var records []sessioncapture.Captured
	for cp.Offset < int64(len(raw)) {
		batch, err := sessioncapture.Read(bytes.NewReader(raw), s, cp, limits, clock)
		if err != nil || batch.Blocked || len(batch.Records) != 1 {
			t.Fatal("current schema continuation failed", err, batch.Gaps)
		}
		// Only JSON checkpoint bytes survive this simulated process boundary.
		wire, err := json.Marshal(batch.Checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		cp = sessioncapture.Checkpoint{}
		if err := json.Unmarshal(wire, &cp); err != nil {
			t.Fatal(err)
		}
		records = append(records, batch.Records...)
	}
	if !reflect.DeepEqual(records, whole.Records) || cp != whole.Checkpoint {
		t.Fatal("restart changed current schema records/checkpoint")
	}
	for _, record := range records {
		if record.Record.ProviderVersion != version {
			t.Fatal("native provider version lost")
		}
		if err := record.Record.VerifyRaw(record.Raw); err != nil {
			t.Fatal(err)
		}
	}
	return whole
}
func TestInstalledClaudeMessageRecapAndRestartCapabilities(t *testing.T) {
	for _, version := range []string{"2.1.296", "2.1.303"} {
		t.Run(version, func(t *testing.T) {
			batch := currentCaptureWithRestarts(t, currentClaudeFixture(version), sessioncapture.Claude, version)
			texts := map[string]bool{}
			recaps := 0
			calls := 0
			results := 0
			for _, captured := range batch.Records {
				r := captured.Record
				if r.Kind == "message" {
					texts[r.Body.Text] = true
				}
				if r.Kind == "native_recap" {
					recaps++
					if r.Body.Text != "Synthetic native persisted recap." {
						t.Fatal("native recap content lost")
					}
				}
				for _, part := range r.Body.Parts {
					if part.Type == "tool_call" {
						calls++
					}
					if part.Type == "tool_result" {
						results++
					}
				}
			}
			if !texts["Synthetic current request."] || !texts["Synthetic current answer."] || recaps != 1 || calls != 1 || results != 1 {
				t.Fatalf("supported shapes missing: recaps=%d calls=%d results=%d", recaps, calls, results)
			}
			capabilities := sessioncapture.Probe(sessioncapture.Claude, version)
			if capabilities.OrdinaryContent.State != "fixture_verified" || capabilities.NativeRecap.State != "fixture_verified" || capabilities.LiveCollection.State != "fixture_verified" {
				t.Fatal("current verified schema capabilities not advertised")
			}
		})
	}
}
func TestInstalledCodexOrdinaryAndRestartCapabilities(t *testing.T) {
	batch := currentCaptureWithRestarts(t, currentCodexFixture(), sessioncapture.Codex, "0.163.0")
	roles := map[string]bool{}
	for _, captured := range batch.Records {
		r := captured.Record
		if r.Kind == "native_recap" {
			t.Fatal("invented native recap")
		}
		if r.Kind == "message" && r.Role != nil {
			roles[*r.Role] = true
		}
	}
	if !roles["user"] || !roles["assistant"] || !roles["developer"] {
		t.Fatal("ordinary response-item roles missing")
	}
	capabilities := sessioncapture.Probe(sessioncapture.Codex, "0.163.0")
	if capabilities.OrdinaryContent.State != "fixture_verified" || capabilities.LiveCollection.State != "fixture_verified" || capabilities.NativeRecap.State != "unsupported" {
		t.Fatal("current Codex capability boundary incorrect")
	}
}
func TestCurrentCompatibilityDoesNotBroadenUnknownOrActivation(t *testing.T) {
	for _, provider := range []string{sessioncapture.Claude, sessioncapture.Codex} {
		if c := sessioncapture.Probe(provider, "uninspected-future"); c.OrdinaryContent.State != "unknown" || c.NativeRecap.State != "unknown" {
			t.Fatal("unknown version gained compatibility")
		}
		version := "2.1.303"
		if provider == sessioncapture.Codex {
			version = "0.163.0"
		}
		c := sessioncapture.Probe(provider, version)
		if c.ConversationAlias.State != "unsupported" || c.SurfaceActivation.State != "unsupported" {
			t.Fatal("generic alias or provider activation gained compatibility")
		}
	}
}
