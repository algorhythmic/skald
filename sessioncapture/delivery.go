package sessioncapture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/algorhythmic/skald/sessionrecord"
)

// DeliveryVersion versions the collector-to-sink envelope independently of
// sessionrecord.Version and Checkpoint.Version.
const DeliveryVersion = 1

// MaxDeliveryBytes bounds a JSON transport envelope, including base64 raw bytes
// and normalization overhead. A producer must use smaller Read limits if needed.
const MaxDeliveryBytes = 256 << 20

var ErrDeliveryVersion = errors.New("unsupported_delivery_version")
var ErrAcknowledgement = errors.New("invalid_delivery_acknowledgement")

// Delivery is an immutable candidate from one source checkpoint to the next.
// Persist the complete envelope before retrying delivery: a fresh Read can have
// different observation timestamps and therefore a different BatchID.
type Delivery struct {
	Version  int        `json:"version"`
	BatchID  string     `json:"batch_id"`
	Source   Source     `json:"source"`
	Previous Checkpoint `json:"previous"`
	Batch    Batch      `json:"batch"`
}

// Acknowledgement confirms atomic acceptance by one named destination only.
// It is not a task acknowledgement, completion claim, or cross-sink checkpoint.
type Acknowledgement struct {
	Version     int        `json:"version"`
	Destination string     `json:"destination"`
	BatchID     string     `json:"batch_id"`
	SourceKey   string     `json:"source_key"`
	Checkpoint  Checkpoint `json:"checkpoint"`
}

// Sink must persist all accepted records, gaps and the candidate checkpoint in
// one atomic operation before returning an acknowledgement. Any error leaves
// acceptance uncertain to the caller; never advance its checkpoint on error.
// Implementations own destination authorization, sole-writer enforcement,
// durable deduplication (if offered), retention and redaction.
type Sink interface {
	Destination() string
	Accept(context.Context, Delivery) (Acknowledgement, error)
}

func NewDelivery(source Source, previous Checkpoint, batch Batch) (Delivery, error) {
	d := Delivery{Version: DeliveryVersion, Source: source, Previous: previous, Batch: batch}
	if err := d.validateBatch(); err != nil {
		return Delivery{}, err
	}
	id, err := d.identity()
	if err != nil {
		return Delivery{}, err
	}
	d.BatchID = id
	return d, nil
}

func (d Delivery) Validate() error {
	if err := d.validateBatch(); err != nil {
		return err
	}
	id, err := d.identity()
	if err != nil {
		return err
	}
	if d.BatchID != id {
		return errors.New("delivery_identity_mismatch")
	}
	return nil
}

// identity uses the pinned typed Go JSON encoding with an empty batch_id.
// Raw JSON stays opaque: encoding/json compacts and escapes it without dropping
// duplicate native keys, sorting native objects, or rounding numeric lexemes.
// The resulting bytes are one field in sessionrecord's canonical key encoding.
func (d Delivery) identity() (string, error) {
	d.BatchID = ""
	wire, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	// The final envelope replaces the empty ID with this fixed-size key.
	if len(wire) > MaxDeliveryBytes-(len("sr1:capture-batch:")+64) {
		return "", errors.New("delivery_too_large")
	}
	return sessionrecord.Key("capture-batch", string(wire)), nil
}

func validateCheckpoint(source Source, cp Checkpoint, initial bool) error {
	if initial && cp == (Checkpoint{}) {
		return nil
	}
	if cp.Version != 1 || cp.SourceKey != source.Key() || cp.Epoch < 0 || cp.Offset < 0 || cp.Ordinal < 0 || cp.ParsedOffset < 0 || cp.ParsedOffset > cp.Offset || !sessionrecord.ValidDigest(cp.PrefixDigest) || cp.ProviderVersion == "" || (cp.Offset > 0 && cp.Generation == "") || (cp.Ordinal > 0 && cp.ConversationID == "") {
		return errors.New("invalid_delivery_checkpoint")
	}
	if source.ConversationID != "" && cp.ConversationID != "" && source.ConversationID != cp.ConversationID {
		return errors.New("checkpoint_conversation_mismatch")
	}
	if cp.Offset == 0 && (cp.Ordinal != 0 || cp.PrefixDigest != sessionrecord.Digest(nil)) {
		return errors.New("invalid_empty_checkpoint")
	}
	if !cp.HasGaps && cp.ParsedOffset != cp.Offset {
		return errors.New("invalid_parsed_checkpoint")
	}
	return nil
}

func (d Delivery) validateBatch() error {
	if d.Version != DeliveryVersion {
		return ErrDeliveryVersion
	}
	if err := d.Source.Validate(); err != nil {
		return err
	}
	for _, s := range []string{d.Source.Namespace, d.Source.Provider, d.Source.StreamID, d.Source.ConversationID, d.Source.ProviderVersion} {
		if !utf8.ValidString(s) {
			return errors.New("invalid_source_encoding")
		}
	}
	if err := validateCheckpoint(d.Source, d.Previous, true); err != nil {
		return err
	}
	cp, prev, batch := d.Batch.Checkpoint, d.Previous, d.Batch
	if err := validateCheckpoint(d.Source, cp, false); err != nil {
		return err
	}
	if batch.Records == nil || batch.Gaps == nil || len(batch.Records) > 10000 || len(batch.Gaps) > 10002 {
		return errors.New("invalid_delivery_arrays")
	}
	if batch.SourceActive && batch.SourceActiveAt.IsZero() {
		return errors.New("source_activity_time_required")
	}
	if cp.Epoch < prev.Epoch || cp.Epoch-prev.Epoch > 1 || (prev.Version == 0 && cp.Epoch != 0) {
		return errors.New("checkpoint_regression")
	}
	baseOffset, baseOrdinal := prev.Offset, prev.Ordinal
	rewrite := cp.Epoch != prev.Epoch
	if rewrite {
		baseOffset, baseOrdinal = 0, 0
		if !cp.HasGaps || cp.ParsedOffset != 0 {
			return errors.New("rewrite_gap_required")
		}
	} else {
		if prev.Generation != "" && cp.Generation != prev.Generation {
			return errors.New("generation_conflict")
		}
		if prev.ConversationID != "" && cp.ConversationID != prev.ConversationID {
			return errors.New("checkpoint_conversation_mismatch")
		}
		if cp.ParsedOffset < prev.ParsedOffset || (prev.HasGaps && (!cp.HasGaps || cp.ParsedOffset != prev.ParsedOffset)) {
			return errors.New("parsed_checkpoint_regression")
		}
	}
	if baseOrdinal > math.MaxInt64-int64(len(batch.Records)) {
		return errors.New("candidate_boundary_overflow")
	}
	boundaries := make(map[int64]int64, len(batch.Records)+1)
	boundaries[baseOrdinal] = baseOffset
	var rawBytes int64
	prefix := sha256.New()
	for i, item := range batch.Records {
		if len(item.Raw) == 0 || bytes.IndexByte(item.Raw, '\n') != len(item.Raw)-1 || len(item.Raw) > 64<<20 {
			return errors.New("invalid_captured_line")
		}
		rec := item.Record
		if err := rec.VerifyRaw(item.Raw); err != nil {
			return fmt.Errorf("delivery_record_%d: %w", i, err)
		}
		if rec.Provider != d.Source.Provider || rec.SourceNamespace != d.Source.Namespace || rec.ConversationKey == nil || *rec.ConversationKey != sessionrecord.Conversation(d.Source.Namespace, cp.ConversationID) || rec.SourceOrder == nil || rec.SourceOrder.StreamGeneration != cp.Generation {
			return errors.New("batch_source_mismatch")
		}
		if rec.SourceOrder.Ordinal != baseOrdinal+int64(i) {
			return errors.New("candidate_order_mismatch")
		}
		if baseOffset > math.MaxInt64-int64(len(item.Raw)) {
			return errors.New("candidate_boundary_overflow")
		}
		baseOffset += int64(len(item.Raw))
		rawBytes += int64(len(item.Raw))
		_, _ = prefix.Write(item.Raw)
		boundaries[baseOrdinal+int64(i)+1] = baseOffset
	}
	if rawBytes > 128<<20 || cp.Offset != baseOffset || cp.Ordinal != baseOrdinal+int64(len(batch.Records)) {
		return errors.New("candidate_boundary_mismatch")
	}
	if (prev.Offset == 0 || rewrite) && cp.PrefixDigest != "sha256:"+hex.EncodeToString(prefix.Sum(nil)) {
		return errors.New("candidate_prefix_mismatch")
	}
	if len(batch.Records) == 0 && !rewrite && prev.Version != 0 && cp.PrefixDigest != prev.PrefixDigest {
		return errors.New("unchanged_prefix_mismatch")
	}
	continuity := 0
	hasGaps, parsedOffset := prev.HasGaps || rewrite, prev.ParsedOffset
	if rewrite {
		parsedOffset = 0
	}
	firstGapOffset := cp.Offset
	for _, gap := range batch.Gaps {
		if gap.Code == "continuity_gap" {
			continuity++
			if !rewrite || gap.Offset != prev.Offset || gap.Ordinal != prev.Ordinal || gap.Generation != prev.Generation {
				return errors.New("invalid_continuity_gap")
			}
			continue
		}
		offset, ok := boundaries[gap.Ordinal]
		if gap.Code == "" || !ok || gap.Offset != offset || (gap.Generation != cp.Generation && !(gap.Generation == "" && gap.Offset == 0)) {
			return errors.New("invalid_delivery_gap")
		}
		// Consumed records with parsing gaps freeze the parsed boundary. A
		// terminal blocking gap has not consumed bytes and does not do so.
		if gap.Ordinal < cp.Ordinal {
			if gap.Offset < firstGapOffset {
				firstGapOffset = gap.Offset
			}
		}
	}
	if (rewrite && continuity != 1) || (!rewrite && continuity != 0) {
		return errors.New("continuity_gap_required")
	}
	if !hasGaps {
		parsedOffset = firstGapOffset
	}
	hasGaps = hasGaps || firstGapOffset < cp.Offset
	if cp.HasGaps != hasGaps || cp.ParsedOffset != parsedOffset {
		return errors.New("candidate_gap_coverage_mismatch")
	}
	return nil
}

func (a Acknowledgement) Validate(destination string, d Delivery) error {
	if destination == "" || a.Version != DeliveryVersion || a.Destination != destination || a.BatchID != d.BatchID || a.SourceKey != d.Source.Key() || a.Checkpoint != d.Batch.Checkpoint {
		return ErrAcknowledgement
	}
	return nil
}

// Deliver checks both sides of the boundary, returning no acknowledgement on
// error, even if a sink supplies one. Only its named destination may advance.
func Deliver(ctx context.Context, sink Sink, d Delivery) (Acknowledgement, error) {
	if err := d.Validate(); err != nil {
		return Acknowledgement{}, err
	}
	destination := sink.Destination()
	if destination == "" {
		return Acknowledgement{}, ErrAcknowledgement
	}
	ack, err := sink.Accept(ctx, d)
	if err != nil {
		return Acknowledgement{}, err
	}
	if err := ack.Validate(destination, d); err != nil {
		return Acknowledgement{}, err
	}
	return ack, nil
}
