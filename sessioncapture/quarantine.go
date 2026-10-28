package sessioncapture

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/algorhythmic/skald/sessionrecord"
)

const QuarantineReason = "redaction_quarantined"
const QuarantinePattern = "redaction_error"

// Quarantine contains only replay coordinates and constant policy identifiers.
// No original bytes, native record ID, original digest, or detector error text.
type Quarantine struct {
	Ordinal      int64  `json:"ordinal"`
	PatternID    string `json:"pattern_id"`
	Reason       string `json:"reason"`
	SourceOffset int64  `json:"source_offset"`
}

func QuarantineRecord(c Captured, offset int64) Captured {
	q := Quarantine{Ordinal: c.Record.SourceOrder.Ordinal, PatternID: QuarantinePattern, Reason: QuarantineReason, SourceOffset: offset}
	wire, _ := json.Marshal(q)
	raw := append(append([]byte{}, wire...), '\n')
	rec := sessionrecord.Record{
		ContractVersion: sessionrecord.Version, SourceNamespace: c.Record.SourceNamespace,
		Provider: c.Record.Provider, ProviderVersion: "unknown", AdapterVersion: AdapterVersion,
		Kind: "opaque_record", NativeKind: QuarantineReason, SourceOrder: c.Record.SourceOrder,
		ConversationKey: c.Record.ConversationKey, OriginScope: c.Record.OriginScope,
		ObservedAt: c.Record.ObservedAt, Body: sessionrecord.Body{State: "opaque"},
		Availability: sessionrecord.Availability{State: "opaque", Reason: QuarantineReason},
		Coverage:     sessionrecord.Coverage{Kind: "unknown"}, Provenance: sessionrecord.Provenance{Kind: "native", Producer: "skald-quarantine"},
		Relations: []sessionrecord.Relation{}, Extensions: map[string]json.RawMessage{"skald_quarantine": wire},
	}
	rec.RecordKey = sessionrecord.Key("quarantine", rec.SourceNamespace, c.Record.SourceOrder.StreamGeneration, strconv.FormatInt(q.Ordinal, 10))
	rec.SourceRevision = sessionrecord.Digest(raw)
	meta := &Redaction{Version: 1, Applied: true, Digest: rec.SourceRevision, NativeBytes: c.NativeLength(), Rules: []string{QuarantinePattern}, Counts: map[string]int64{QuarantinePattern: 1}}
	rec.Extensions[redactionExtension], _ = json.Marshal(meta)
	return Captured{Record: rec, Raw: raw, Redaction: meta}
}

func (c Captured) QuarantineMetadata() (Quarantine, bool) {
	var q Quarantine
	raw, ok := c.Record.Extensions["skald_quarantine"]
	if !ok || json.Unmarshal(raw, &q) != nil {
		return q, false
	}
	return q, true
}
func (c Captured) verifyQuarantine() error {
	fail := errors.New("invalid_quarantine_record")
	q, ok := c.QuarantineMetadata()
	if !ok || q.Ordinal < 0 || q.SourceOffset < 0 || q.PatternID != QuarantinePattern || q.Reason != QuarantineReason || c.Record.SourceOrder == nil || q.Ordinal != c.Record.SourceOrder.Ordinal {
		return fail
	}
	expected := QuarantineRecord(c, q.SourceOffset)
	// Enforce the entire content-free representation, not just its marker.
	a, _ := json.Marshal(c)
	b, _ := json.Marshal(expected)
	if !bytes.Equal(a, b) {
		return fail
	}
	return nil
}

// ValidateQuarantineBoundary binds replay metadata to the accepted source range
// and its transactional gap receipt, rather than trusting transport metadata.
func ValidateQuarantineBoundary(c Captured, offset int64, gaps []Gap) error {
	q, ok := c.QuarantineMetadata()
	if !ok {
		return nil
	}
	if c.Record.SourceOrder == nil || q.SourceOffset != offset {
		return errors.New("invalid_quarantine_boundary")
	}
	for _, gap := range gaps {
		if gap.Code == QuarantineReason && gap.Offset == offset && gap.Ordinal == q.Ordinal && gap.Generation == c.Record.SourceOrder.StreamGeneration {
			return nil
		}
	}
	return errors.New("quarantine_gap_required")
}
