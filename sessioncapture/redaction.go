package sessioncapture

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/algorhythmic/skald/internal/secretredact"
	"github.com/algorhythmic/skald/sessionrecord"
	"sort"
)

// Redaction is sanitized representation v1. Digest identifies sanitized native
// bytes, never a per-secret hash. NativeBytes is exclusively a source boundary;
// the native prefix digest is verified by the collector, not reconstructed from
// redacted content by the sink. Counts include native and normalized surfaces.
type Redaction struct {
	Version     int              `json:"version"`
	Applied     bool             `json:"applied"`
	Digest      string           `json:"digest"`
	NativeBytes int64            `json:"native_bytes"`
	Rules       []string         `json:"rules"`
	Counts      map[string]int64 `json:"counts"`
}

const SanitizedDeliveryVersion = 2
const redactionExtension = "skald_redaction"

func (c Captured) NativeLength() int64 {
	if c.Redaction != nil {
		return c.Redaction.NativeBytes
	}
	return int64(len(c.Raw))
}
func (c Captured) Verify() error {
	if err := c.Record.VerifyRaw(c.Raw); err != nil {
		return err
	}
	if c.Redaction == nil {
		return nil
	}
	m := c.Redaction
	if m.Version != 1 || !m.Applied || m.NativeBytes < 1 || m.NativeBytes > 64<<20 || m.Digest != c.Record.SourceRevision || len(m.Rules) == 0 || len(m.Rules) != len(m.Counts) {
		return errors.New("invalid_sanitized_record")
	}
	known := secretredact.Counts()
	for i, id := range m.Rules {
		if _, ok := known[id]; (!ok && id != QuarantinePattern) || m.Counts[id] < 1 || (i > 0 && m.Rules[i-1] >= id) {
			return errors.New("invalid_sanitized_record")
		}
	}
	stored, err := json.Marshal(m)
	if err != nil || !bytes.Equal(stored, c.Record.Extensions[redactionExtension]) {
		return errors.New("invalid_sanitized_record")
	}
	if _, exists := c.Record.Extensions["skald_quarantine"]; exists || m.Counts[QuarantinePattern] > 0 {
		return c.verifyQuarantine()
	}
	return nil
}

// Sanitize is copy-on-write and idempotent. Detector errors return a constant
// error; positive matches always produce a stored record and native boundaries.
func Sanitize(c Captured) (result Captured, failure error) {
	defer func() {
		if recover() != nil {
			result = Captured{}
			failure = secretredact.ErrSensitive
		}
	}()
	if err := c.Verify(); err != nil {
		return Captured{}, err
	}
	if c.Redaction != nil {
		return c, nil
	}
	counts := secretredact.Counts()
	if err := secretredact.Inspect(c.Raw, counts); err != nil {
		return Captured{}, secretredact.ErrSensitive
	}
	wire, err := json.Marshal(c.Record)
	if err != nil {
		return Captured{}, secretredact.ErrSensitive
	}
	if err := secretredact.Inspect(wire, counts); err != nil {
		return Captured{}, secretredact.ErrSensitive
	}
	ids := []string{}
	for id, n := range counts {
		if n > 0 {
			ids = append(ids, id)
		} else {
			delete(counts, id)
		}
	}
	if len(ids) == 0 {
		return c, nil
	}
	sort.Strings(ids)
	raw := secretredact.JSON(c.Raw, nil)
	if !bytes.Equal(raw, c.Raw) {
		raw = append(bytes.TrimRight(raw, "\r\n"), '\n')
	}
	recWire := secretredact.JSON(wire, nil)
	rec, err := sessionrecord.Decode(recWire)
	if err != nil {
		return Captured{}, failureForRules(counts)
	}
	rec.SourceRevision = sessionrecord.Digest(raw)
	if rec.RawRef != nil {
		rec.RawRef = &sessionrecord.Ref{RecordKey: rec.RecordKey, SourceRevision: rec.SourceRevision}
	}
	meta := &Redaction{Version: 1, Applied: true, Digest: rec.SourceRevision, NativeBytes: int64(len(c.Raw)), Rules: ids, Counts: counts}
	if rec.Extensions == nil {
		rec.Extensions = map[string]json.RawMessage{}
	}
	rec.Extensions[redactionExtension], _ = json.Marshal(meta)
	result = Captured{Record: rec, Raw: raw, Redaction: meta}
	check, _ := json.Marshal(rec)
	if err := residualFailure(raw, check); err != nil {
		return Captured{}, err
	}
	if err := result.Verify(); err != nil {
		return Captured{}, failureForRules(counts)
	}
	return result, nil
}
func SanitizeBatch(batch Batch) (Batch, error) {
	out := batch
	out.Checkpoint = PublicCheckpoint(batch.Checkpoint)
	if batch.Records == nil {
		return out, nil
	}
	out.Records = make([]Captured, len(batch.Records))
	offset := batch.Checkpoint.Offset
	for _, c := range batch.Records {
		offset -= c.NativeLength()
	}
	if offset < 0 {
		return Batch{}, errors.New("invalid_candidate_boundary")
	}
	if batch.Gaps != nil {
		out.Gaps = append([]Gap{}, batch.Gaps...)
	}
	for i, c := range batch.Records {
		if batch.Checkpoint.ConversationID != out.Checkpoint.ConversationID && c.Record.ConversationKey != nil {
			conv := sessionrecord.Conversation(c.Record.SourceNamespace, out.Checkpoint.ConversationID)
			c.Record.ConversationKey = &conv
			if c.Record.OriginScope.Kind == "conversation" {
				c.Record.OriginScope.Key = conv
			}
		}
		if err := c.Verify(); err != nil {
			return Batch{}, err
		}
		if c.Record.SourceOrder == nil {
			return Batch{}, errors.New("candidate_order_required")
		}
		next, err := Sanitize(c)
		if err != nil {
			if batch.Gaps == nil {
				return Batch{}, errors.New("invalid_delivery_arrays")
			}
			next = QuarantineRecord(c, offset, err)
			out.Gaps = append(out.Gaps, Gap{Code: QuarantineReason, Offset: offset, Ordinal: c.Record.SourceOrder.Ordinal, Generation: c.Record.SourceOrder.StreamGeneration})
			if !out.Checkpoint.HasGaps || offset < out.Checkpoint.ParsedOffset {
				out.Checkpoint.ParsedOffset = offset
			}
			out.Checkpoint.HasGaps = true
		}
		out.Records[i] = next
		offset += c.NativeLength()
	}
	return out, nil
}
