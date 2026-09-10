package sessionrecord_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"skald/sessionrecord"
)

func TestCanonicalVectors(t *testing.T) {
	b, err := os.ReadFile("../testdata/identity-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Domain       string
		Fields       []string
		CanonicalHex string `json:"canonical_hex"`
		Key          string
	}
	if err := json.Unmarshal(b, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		if got := hex.EncodeToString(sessionrecord.Canonical(v.Domain, v.Fields...)); got != v.CanonicalHex {
			t.Fatalf("canonical vector: %s", got)
		}
		if got := sessionrecord.Key(v.Domain, v.Fields...); got != v.Key {
			t.Fatalf("hash vector: %s", got)
		}
	}
}

func validRecord() sessionrecord.Record {
	r := sessionrecord.Record{ContractVersion: 1, SourceNamespace: "fixture:source", OriginScope: sessionrecord.Scope{Kind: "source", Key: "fixture:source"}, RecordKey: "fixture:record", SourceRevision: sessionrecord.Digest([]byte("raw\n")), Provider: "fixture", ProviderVersion: "unknown", AdapterVersion: "fixture-v1", Kind: "native_note", NativeKind: "note", ObservedAt: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), Coverage: sessionrecord.Coverage{Kind: "unknown"}, Provenance: sessionrecord.Provenance{Kind: "native", Producer: "unknown"}, Relations: []sessionrecord.Relation{}, Availability: sessionrecord.Availability{State: "available"}}
	r.RawRef = &sessionrecord.Ref{RecordKey: r.RecordKey, SourceRevision: r.SourceRevision}
	return r
}

func TestContractRejectsInvalidRecords(t *testing.T) {
	for name, mutate := range map[string]func(*sessionrecord.Record){
		"major":            func(r *sessionrecord.Record) { r.ContractVersion = 2 },
		"digest":           func(r *sessionrecord.Record) { r.SourceRevision = "fixture:digest" },
		"inferred_session": func(r *sessionrecord.Record) { s := "invented"; r.ConversationKey = &s },
		"missing_session":  func(r *sessionrecord.Record) { r.OriginScope.Kind = "conversation" },
		"invalid_coverage": func(r *sessionrecord.Record) {
			r.Coverage.Through = &sessionrecord.Order{StreamGeneration: "g", Ordinal: 2}
		},
		"missing_reason":   func(r *sessionrecord.Record) { r.Availability.State = "unavailable" },
		"raw_mismatch":     func(r *sessionrecord.Record) { r.RawRef.RecordKey = "different" },
		"missing_producer": func(r *sessionrecord.Record) { r.Provenance.Producer = "" },
		"null_relations":   func(r *sessionrecord.Record) { r.Relations = nil },
	} {
		t.Run(name, func(t *testing.T) {
			r := validRecord()
			mutate(&r)
			if r.Validate() == nil {
				t.Fatal("accepted invalid record")
			}
		})
	}
	r := validRecord()
	if err := r.VerifyRaw([]byte("raw\n")); err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyRaw([]byte("raw")); err == nil {
		t.Fatal("line terminator must affect revision")
	}
	b, _ := json.Marshal(r)
	if _, err := sessionrecord.Decode(b); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	delete(fields, "role")
	bad, _ := json.Marshal(fields)
	if _, err := sessionrecord.Decode(bad); err == nil {
		t.Fatal("accepted omitted nullable field")
	}
	fields["role"] = nil
	fields["surprise"] = true
	bad, _ = json.Marshal(fields)
	if _, err := sessionrecord.Decode(bad); err == nil {
		t.Fatal("accepted undeclared extension")
	}
	delete(fields, "surprise")
	fields["body"] = nil
	bad, _ = json.Marshal(fields)
	if _, err := sessionrecord.Decode(bad); err == nil {
		t.Fatal("accepted null body")
	}
}

func TestNamespaceOverrideAndOccurrences(t *testing.T) {
	a, err := sessionrecord.Namespace("claude_code", "host", "/old", "", "configured:source")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := sessionrecord.Namespace("claude_code", "host", "/new", "", "configured:source")
	if a != b {
		t.Fatal("explicit namespace relocation changed identity")
	}
	if _, err := sessionrecord.Namespace("claude_code", "", "/root", "", ""); err == nil {
		t.Fatal("missing stable host accepted")
	}
	scope := sessionrecord.Scope{Kind: "conversation", Key: "session"}
	if sessionrecord.OrdinalRecord(scope, "g", 1) == sessionrecord.OrdinalRecord(scope, "g", 2) {
		t.Fatal("occurrences merged")
	}
	if sessionrecord.NativeRecord(scope, "same") == sessionrecord.NativeRecord(sessionrecord.Scope{Kind: "conversation", Key: "other"}, "same") {
		t.Fatal("cross-session collision")
	}
}
