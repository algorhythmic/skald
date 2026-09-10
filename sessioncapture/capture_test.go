package sessioncapture_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"skald/sessioncapture"
	"skald/sessionrecord"
)

var clock = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func source(provider string) sessioncapture.Source {
	return sessioncapture.Source{Namespace: "fixture:" + provider, Provider: provider, StreamID: "stream-1"}
}
func fixture(t *testing.T, provider string) []byte {
	t.Helper()
	p := "claude"
	if provider == sessioncapture.Codex {
		p = "codex"
	}
	b, err := os.ReadFile("../testdata/" + p + "/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func read(t *testing.T, b []byte, s sessioncapture.Source, cp sessioncapture.Checkpoint) sessioncapture.Batch {
	t.Helper()
	out, err := sessioncapture.Read(bytes.NewReader(b), s, cp, sessioncapture.DefaultLimits(), clock)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestIndependentConsumersConform(t *testing.T) {
	schemaBytes, err := os.ReadFile("../schemas/session-record-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{sessioncapture.Claude, sessioncapture.Codex} {
		t.Run(provider, func(t *testing.T) {
			direct := read(t, fixture(t, provider), source(provider), sessioncapture.Checkpoint{})
			if direct.Blocked || direct.Pending || direct.Checkpoint.HasGaps {
				t.Fatalf("unexpected incomplete fixture: %+v", direct.Gaps)
			}
			for _, item := range direct.Records {
				wire, err := json.Marshal(item.Record)
				if err != nil {
					t.Fatal(err)
				}
				var instance any
				if err := json.Unmarshal(wire, &instance); err != nil {
					t.Fatal(err)
				}
				if err := resolved.Validate(instance); err != nil {
					t.Fatalf("JSON schema conformance: %v", err)
				}
				transport, err := sessioncapture.DecodeEnvelope(wire, item.Raw)
				if err != nil {
					t.Fatal(err)
				}
				roundtrip, _ := json.Marshal(transport)
				if !bytes.Equal(wire, roundtrip) {
					t.Fatal("direct and transport consumer records differ")
				}
				if item.Record.Coverage.Kind != "unknown" {
					t.Fatal("invented summary coverage")
				}
			}
			if provider == sessioncapture.Claude {
				if direct.Records[3].Record.Kind != "native_recap" {
					t.Fatal("native recap missing")
				}
				a, b := direct.Records[5], direct.Records[6]
				if a.Record.SourceRevision != b.Record.SourceRevision || a.Record.RecordKey == b.Record.RecordKey {
					t.Fatal("same bytes must be distinct occurrences")
				}
				if direct.Records[7].Record.Availability.State != "opaque" {
					t.Fatal("unknown record not preserved")
				}
			} else {
				last := direct.Records[len(direct.Records)-1].Record
				if last.Kind != "compaction_marker" || last.Availability.State != "opaque" || last.Body.Text != "" {
					t.Fatal("opaque compaction became a recap")
				}
				if direct.Records[7].Record.Body.State != "idle" {
					t.Fatal("turn complete is idle evidence")
				}
				for _, r := range direct.Records {
					if r.Record.Body.State == "ended" {
						t.Fatal("fabricated conversation end")
					}
				}
			}
		})
	}
}

func TestPartialWritesRetryAndBatching(t *testing.T) {
	b := fixture(t, sessioncapture.Claude)
	s := source(sessioncapture.Claude)
	cut := bytes.IndexByte(b, '\n') + 1
	partial := read(t, b[:cut+20], s, sessioncapture.Checkpoint{})
	if !partial.Pending || partial.Checkpoint.Offset != int64(cut) || len(partial.Records) != 1 {
		t.Fatal("partial line advanced checkpoint")
	}
	tail := read(t, b, s, partial.Checkpoint)
	whole := read(t, b, s, sessioncapture.Checkpoint{})
	combined := append(partial.Records, tail.Records...)
	if !reflect.DeepEqual(combined, whole.Records) {
		t.Fatal("resumed records differ from one-pass capture")
	}
	retry := read(t, b, s, whole.Checkpoint)
	if len(retry.Records) != 0 || retry.Checkpoint != whole.Checkpoint {
		t.Fatal("duplicate delivery advanced state")
	}
	limits := sessioncapture.DefaultLimits()
	limits.Records = 1
	one, err := sessioncapture.Read(bytes.NewReader(b), s, sessioncapture.Checkpoint{}, limits, clock)
	if err != nil || !one.More || len(one.Records) != 1 {
		t.Fatal("record bound failed")
	}
	limits.Records = 100
	limits.RecordBytes = cut + 1
	limits.BatchBytes = cut + 1
	bounded, err := sessioncapture.Read(bytes.NewReader(b), s, sessioncapture.Checkpoint{}, limits, clock)
	if err != nil || len(bounded.Records) != 1 || bounded.Checkpoint.Offset != int64(cut) {
		t.Fatal("byte bound advanced uncaptured records")
	}
}

func TestRotationRewriteAndRevisionIdentity(t *testing.T) {
	b := fixture(t, sessioncapture.Claude)
	s := source(sessioncapture.Claude)
	original := read(t, b, s, sessioncapture.Checkpoint{})
	// A new reader represents another file at a new path; verified bytes and the
	// configured logical stream token preserve identity across rotation.
	rotated := read(t, append(append([]byte{}, b...), []byte("{\"type\":\"future_record\",\"sessionId\":\"synthetic-claude\"}\n")...), s, original.Checkpoint)
	if rotated.Checkpoint.Generation != original.Checkpoint.Generation || len(rotated.Gaps) != 0 {
		t.Fatal("prefix-preserving rotation lost continuity")
	}
	rewritten := bytes.Replace(b, []byte("Keep archive history"), []byte("Retain archive history"), 1)
	changed := read(t, rewritten, s, original.Checkpoint)
	if !changed.Checkpoint.HasGaps || len(changed.Gaps) != 1 || changed.Gaps[0].Code != "continuity_gap" {
		t.Fatal("rewrite not diagnosed")
	}
	if changed.Checkpoint.Generation == original.Checkpoint.Generation {
		t.Fatal("ambiguous rewrite reused generation")
	}
	if changed.Records[0].Record.RecordKey != original.Records[0].Record.RecordKey || changed.Records[0].Record.SourceRevision == original.Records[0].Record.SourceRevision {
		t.Fatal("native ID revision not preserved")
	}
	if changed.Records[5].Record.RecordKey == original.Records[5].Record.RecordKey {
		t.Fatal("ID-less record continuity invented")
	}
	wrong := s
	wrong.StreamID = "other"
	if _, err := sessioncapture.Read(bytes.NewReader(b), wrong, original.Checkpoint, sessioncapture.DefaultLimits(), clock); err == nil {
		t.Fatal("cross-stream checkpoint accepted")
	}
}

func TestMalformedAndOversizedInput(t *testing.T) {
	b := fixture(t, sessioncapture.Claude)
	end := bytes.IndexByte(b, '\n') + 1
	withBad := append(append(append([]byte{}, b[:end]...), []byte("not JSON\n")...), b[end:]...)
	out := read(t, withBad, source(sessioncapture.Claude), sessioncapture.Checkpoint{})
	if len(out.Gaps) != 1 || out.Gaps[0].Code != "malformed_json" || !out.Checkpoint.HasGaps || out.Checkpoint.ParsedOffset != int64(end) {
		t.Fatal("missing retained parse gap")
	}
	if string(out.Records[1].Raw) != "not JSON\n" || out.Records[1].Record.Kind != "opaque_record" {
		t.Fatal("malformed input not retained")
	}
	if out.Checkpoint.Offset != int64(len(withBad)) {
		t.Fatal("quarantined stream did not continue")
	}
	limits := sessioncapture.DefaultLimits()
	limits.RecordBytes = 32
	blocked, err := sessioncapture.Read(bytes.NewReader(b), source(sessioncapture.Claude), sessioncapture.Checkpoint{}, limits, clock)
	if err != nil || !blocked.Blocked || blocked.Checkpoint.Offset != 0 || blocked.Gaps[0].Code != "record_too_large" {
		t.Fatal("oversized content silently skipped")
	}
}

func TestIdentityRequiredAndNativeAssertion(t *testing.T) {
	b := []byte("{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":\"hello\"}}\n")
	s := source(sessioncapture.Claude)
	noID := read(t, b, s, sessioncapture.Checkpoint{})
	if !noID.Blocked || noID.Checkpoint.Offset != 0 {
		t.Fatal("invented session from filename or content")
	}
	s.ConversationID = "explicit-session"
	if out := read(t, b, s, sessioncapture.Checkpoint{}); out.Blocked {
		t.Fatal("explicit source assertion failed")
	}
	conflict := read(t, fixture(t, sessioncapture.Claude), s, sessioncapture.Checkpoint{})
	if !conflict.Blocked || len(conflict.Records) != 0 || conflict.Gaps[0].Code != "conversation_identity_mismatch" {
		t.Fatal("native ID conflict ignored")
	}
}

func TestCapabilitiesAndDiscovery(t *testing.T) {
	c := sessioncapture.Probe(sessioncapture.Codex, "0.153.4")
	if c.NativeRecap.State != "unsupported" || c.LiveCollection.State != "fixture_verified" {
		t.Fatal("overclaimed capability")
	}
	if c := sessioncapture.Probe(sessioncapture.Claude, "future"); c.NativeRecap.State != "unknown" {
		t.Fatal("future provider version overclaimed")
	}
	root := t.TempDir()
	outside := t.TempDir()
	for p, body := range map[string]string{filepath.Join(root, "a.jsonl"): "a", filepath.Join(root, "notes.md"): "b", filepath.Join(outside, "private.jsonl"): "c"} {
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	paths, err := sessioncapture.Discover(root, 1)
	if err != nil || len(paths) != 1 || !strings.HasSuffix(paths[0], "a.jsonl") {
		t.Fatal("discovery crossed configured root")
	}
	if err := os.WriteFile(filepath.Join(root, "b.jsonl"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := sessioncapture.Discover(root, 1); err == nil {
		t.Fatal("discovery truncation was silent")
	}
}

func FuzzCapture(f *testing.F) {
	f.Add([]byte("{\"type\":\"future\"}\n"))
	f.Add([]byte("\xff\x00\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<16 {
			t.Skip()
		}
		s := source(sessioncapture.Claude)
		s.ConversationID = "asserted"
		out, err := sessioncapture.Read(bytes.NewReader(b), s, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), clock)
		if err != nil {
			return
		}
		if out.Checkpoint.Offset > int64(len(b)) {
			t.Fatal("advanced past input")
		}
		for _, r := range out.Records {
			if err := r.Record.VerifyRaw(r.Raw); err != nil {
				t.Fatal(err)
			}
		}
		if out.Checkpoint.Offset > 0 && b[out.Checkpoint.Offset-1] != '\n' {
			t.Fatal("checkpoint not at record boundary")
		}
		if out.Checkpoint.PrefixDigest != sessionrecord.Digest(b[:out.Checkpoint.Offset]) {
			t.Fatal("prefix digest mismatch")
		}
	})
}

type mutatingSource struct {
	*bytes.Reader
	replacement []byte
	seeks       int
}

func (m *mutatingSource) Seek(offset int64, whence int) (int64, error) {
	m.seeks++
	if m.seeks == 2 {
		m.Reader = bytes.NewReader(m.replacement)
	}
	return m.Reader.Seek(offset, whence)
}

func TestConcurrentRewriteRejectsCandidateBatch(t *testing.T) {
	b := fixture(t, sessioncapture.Claude)
	changed := bytes.Replace(b, []byte("Keep"), []byte("Drop"), 1)
	r := &mutatingSource{Reader: bytes.NewReader(b), replacement: changed}
	var _ io.ReadSeeker = r
	out, err := sessioncapture.Read(r, source(sessioncapture.Claude), sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), clock)
	if err == nil || err.Error() != "source_changed_during_read" || len(out.Records) != 0 {
		t.Fatal("concurrent rewrite produced a committable batch")
	}
}
