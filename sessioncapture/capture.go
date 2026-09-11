// Package sessioncapture reads configured provider sources without durable writes,
// hooks, model calls, application state or consumer-owned cursor commits.
package sessioncapture

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/algorhythmic/skald/sessionrecord"
)

const AdapterVersion = "jsonl-v1.0.0"
const (
	Claude = "claude_code"
	Codex  = "codex"
	Devin  = "devin"
)

type Source struct {
	Namespace string `json:"namespace"`
	Provider  string `json:"provider"`
	// StreamID is a persisted logical stream token, shared on deliberate relocation.
	// A collector path is not a stream identity and is never substituted here.
	StreamID        string `json:"stream_id"`
	ConversationID  string `json:"conversation_id,omitempty"`
	ProviderVersion string `json:"provider_version,omitempty"`
}
type Checkpoint struct {
	Version         int    `json:"version"`
	SourceKey       string `json:"source_key"`
	Generation      string `json:"generation"`
	Epoch           int64  `json:"epoch"`
	Offset          int64  `json:"offset"`
	ParsedOffset    int64  `json:"parsed_offset"`
	Ordinal         int64  `json:"ordinal"`
	PrefixDigest    string `json:"prefix_digest"`
	ConversationID  string `json:"conversation_id"`
	ProviderVersion string `json:"provider_version"`
	HasGaps         bool   `json:"has_gaps"`
}
type Limits struct {
	Records     int
	RecordBytes int
	BatchBytes  int
}

func DefaultLimits() Limits { return Limits{Records: 256, RecordBytes: 4 << 20, BatchBytes: 16 << 20} }

type Gap struct {
	Code       string `json:"code"`
	Offset     int64  `json:"offset"`
	Ordinal    int64  `json:"ordinal"`
	Generation string `json:"generation"`
}
type Captured struct {
	Record sessionrecord.Record `json:"record"`
	// Raw includes the exact line terminator. JSON encoding carries bytes as base64.
	Raw []byte `json:"raw"`
}
type Batch struct {
	Records    []Captured `json:"records"`
	Gaps       []Gap      `json:"gaps"`
	Checkpoint Checkpoint `json:"checkpoint"`
	Pending    bool       `json:"pending_partial_line"`
	More       bool       `json:"more"`
	Blocked    bool       `json:"blocked"`
	// SourceActive marks a live source the collector deliberately deferred —
	// Devin's store rewrites rows while its WAL is hot. SourceActiveAt is the
	// observed write time, projected as working evidence, not a record.
	SourceActive   bool      `json:"source_active,omitempty"`
	SourceActiveAt time.Time `json:"source_active_at,omitempty"`
}

func sourceKey(s Source) string {
	return sessionrecord.Key("stream", s.Namespace, s.Provider, s.StreamID)
}

// Key identifies this configured logical stream independently of its locator.
func (s Source) Key() string { return sourceKey(s) }

func (s Source) Validate() error { return s.validate() }
func (s Source) validate() error {
	if s.Namespace == "" || s.StreamID == "" {
		return errors.New("namespace_and_stream_id_required")
	}
	if s.Provider != Claude && s.Provider != Codex && s.Provider != Devin {
		return errors.New("unsupported_provider")
	}
	return nil
}

// Read returns a candidate checkpoint; only the consumer can commit it with all
// records and gaps. Prefix verification is deliberately conservative in L0: it
// re-hashes captured bytes with bounded memory before accepting continuation.
func Read(input io.ReadSeeker, source Source, previous Checkpoint, limits Limits, now time.Time) (Batch, error) {
	out := Batch{Records: []Captured{}, Gaps: []Gap{}}
	if err := source.validate(); err != nil {
		return out, err
	}
	if now.IsZero() {
		return out, errors.New("observation_time_required")
	}
	if limits.Records < 1 || limits.Records > 10000 || limits.RecordBytes < 1 || limits.RecordBytes > 64<<20 || limits.BatchBytes < limits.RecordBytes || limits.BatchBytes > 128<<20 {
		return out, errors.New("invalid_limits")
	}
	cp := previous
	if cp.Version != 0 {
		if cp.Version != 1 || cp.SourceKey != sourceKey(source) || cp.Offset < 0 || cp.Ordinal < 0 || cp.Epoch < 0 || cp.ParsedOffset < 0 || cp.ParsedOffset > cp.Offset || !sessionrecord.ValidDigest(cp.PrefixDigest) || (cp.Offset > 0 && cp.Generation == "") {
			return out, errors.New("invalid_checkpoint")
		}
		if source.ConversationID != "" && cp.ConversationID != "" && cp.ConversationID != source.ConversationID {
			return out, errors.New("checkpoint_conversation_mismatch")
		}
	} else {
		if cp != (Checkpoint{}) {
			return out, errors.New("invalid_checkpoint")
		}
		cp = Checkpoint{Version: 1, SourceKey: sourceKey(source), ConversationID: source.ConversationID, ProviderVersion: source.ProviderVersion, PrefixDigest: sessionrecord.Digest(nil)}
	}
	if cp.ProviderVersion == "" {
		cp.ProviderVersion = "unknown"
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return out, err
	}
	h := sha256.New()
	n, err := io.CopyN(h, input, cp.Offset)
	if err != nil && err != io.EOF {
		return out, err
	}
	if n != cp.Offset || "sha256:"+hex.EncodeToString(h.Sum(nil)) != cp.PrefixDigest {
		out.Gaps = append(out.Gaps, Gap{Code: "continuity_gap", Offset: cp.Offset, Ordinal: cp.Ordinal, Generation: cp.Generation})
		cp = Checkpoint{Version: 1, SourceKey: sourceKey(source), Epoch: cp.Epoch + 1, ConversationID: source.ConversationID, ProviderVersion: source.ProviderVersion, PrefixDigest: sessionrecord.Digest(nil), HasGaps: true}
		if cp.ProviderVersion == "" {
			cp.ProviderVersion = "unknown"
		}
		h.Reset()
		if _, err := input.Seek(0, io.SeekStart); err != nil {
			return out, err
		}
	}
	reader := bufio.NewReaderSize(input, 64<<10)
	batchBytes := 0
	for len(out.Records) < limits.Records {
		line, complete, oversized, err := readLine(reader, limits.RecordBytes)
		if err != nil {
			return out, err
		}
		if oversized {
			out.Gaps = append(out.Gaps, Gap{Code: "record_too_large", Offset: cp.Offset, Ordinal: cp.Ordinal, Generation: cp.Generation})
			out.Blocked = true
			break
		}
		if !complete {
			out.Pending = len(line) > 0
			break
		}
		if batchBytes+len(line) > limits.BatchBytes {
			out.More = true
			break
		}
		if cp.Generation == "" {
			cp.Generation = sessionrecord.Key("generation", cp.SourceKey, strconv.FormatInt(cp.Epoch, 10), sessionrecord.Digest(line))
		}
		r, code, err := normalize(line, source, &cp, now)
		if err != nil {
			out.Gaps = append(out.Gaps, Gap{Code: err.Error(), Offset: cp.Offset, Ordinal: cp.Ordinal, Generation: cp.Generation})
			out.Blocked = true
			break
		}
		if code != "" {
			out.Gaps = append(out.Gaps, Gap{Code: code, Offset: cp.Offset, Ordinal: cp.Ordinal, Generation: cp.Generation})
			cp.HasGaps = true
		}
		if err := r.VerifyRaw(line); err != nil {
			return out, fmt.Errorf("adapter_contract: %w", err)
		}
		out.Records = append(out.Records, Captured{Record: r, Raw: line})
		_, _ = h.Write(line)
		cp.Offset += int64(len(line))
		cp.Ordinal++
		batchBytes += len(line)
		if !cp.HasGaps {
			cp.ParsedOffset = cp.Offset
		}
	}
	if !out.Blocked && !out.Pending && !out.More && len(out.Records) == limits.Records {
		_, err := reader.Peek(1)
		out.More = err == nil
		if err != nil && err != io.EOF {
			return out, err
		}
	}
	cp.PrefixDigest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	// A native file may be replaced or rewritten while parsing. Reject the whole
	// candidate batch unless its exact captured prefix still matches at readback.
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return out, err
	}
	verify := sha256.New()
	if n, err := io.CopyN(verify, input, cp.Offset); err != nil || n != cp.Offset {
		return Batch{}, errors.New("source_changed_during_read")
	}
	if "sha256:"+hex.EncodeToString(verify.Sum(nil)) != cp.PrefixDigest {
		return Batch{}, errors.New("source_changed_during_read")
	}
	out.Checkpoint = cp
	return out, nil
}

func readLine(r *bufio.Reader, maxBytes int) ([]byte, bool, bool, error) {
	var line []byte
	for {
		fragment, err := r.ReadSlice('\n')
		if len(line)+len(fragment) > maxBytes {
			return nil, false, true, nil
		}
		line = append(line, fragment...)
		switch err {
		case nil:
			return line, true, false, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			return line, false, false, nil
		default:
			return nil, false, false, err
		}
	}
}

// DecodeEnvelope is the transport consumer harness: byte and envelope validation
// use the same contract without requiring a Skald archive or daemon.
func DecodeEnvelope(data []byte, raw []byte) (sessionrecord.Record, error) {
	r, err := sessionrecord.Decode(bytes.TrimSpace(data))
	if err != nil {
		return r, err
	}
	return r, r.VerifyRaw(raw)
}
