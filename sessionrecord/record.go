package sessionrecord

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"time"
)

const Version = 1

type Scope struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}
type Order struct {
	StreamGeneration string `json:"stream_generation"`
	Ordinal          int64  `json:"ordinal"`
}
type Coverage struct {
	Kind    string `json:"kind"`
	Through *Order `json:"through,omitempty"`
	Start   *Order `json:"start,omitempty"`
	End     *Order `json:"end,omitempty"`
}
type Provenance struct {
	Kind     string `json:"kind"`
	Producer string `json:"producer"`
}
type Availability struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}
type Relation struct {
	Type     string `json:"type"`
	Target   Scope  `json:"target"`
	Evidence string `json:"evidence"`
}
type Ref struct {
	RecordKey      string `json:"record_key"`
	SourceRevision string `json:"source_revision"`
}
type Part struct {
	Type string          `json:"type"`
	Text string          `json:"text,omitempty"`
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}
type Body struct {
	Text  string `json:"text,omitempty"`
	Parts []Part `json:"parts,omitempty"`
	State string `json:"state,omitempty"`
}

type Record struct {
	ContractVersion int                        `json:"contract_version"`
	SourceNamespace string                     `json:"source_namespace"`
	OriginScope     Scope                      `json:"origin_scope"`
	ConversationKey *string                    `json:"conversation_key"`
	RecordKey       string                     `json:"record_key"`
	SourceRevision  string                     `json:"source_revision"`
	Provider        string                     `json:"provider"`
	ProviderVersion string                     `json:"provider_version"`
	AdapterVersion  string                     `json:"adapter_version"`
	Kind            string                     `json:"kind"`
	NativeKind      string                     `json:"native_kind"`
	Role            *string                    `json:"role"`
	Channel         *string                    `json:"channel"`
	SourceTime      *time.Time                 `json:"source_time"`
	SourceOrder     *Order                     `json:"source_order"`
	ObservedAt      time.Time                  `json:"observed_at"`
	Body            Body                       `json:"body"`
	RawRef          *Ref                       `json:"raw_ref"`
	Coverage        Coverage                   `json:"coverage"`
	Provenance      Provenance                 `json:"provenance"`
	Relations       []Relation                 `json:"relations"`
	Availability    Availability               `json:"availability"`
	Extensions      map[string]json.RawMessage `json:"extensions,omitempty"`
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func ValidDigest(s string) bool { return digestPattern.MatchString(s) }
func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func validOrder(o *Order) bool { return o != nil && o.StreamGeneration != "" && o.Ordinal >= 0 }

func (r Record) Validate() error {
	if r.ContractVersion != Version {
		return errors.New("unsupported_contract_version")
	}
	if r.SourceNamespace == "" || r.RecordKey == "" || !ValidDigest(r.SourceRevision) ||
		r.Provider == "" || r.ProviderVersion == "" || r.AdapterVersion == "" || r.NativeKind == "" || r.ObservedAt.IsZero() {
		return errors.New("invalid_required_field")
	}
	if !oneOf(r.OriginScope.Kind, "conversation", "project", "source") || r.OriginScope.Key == "" {
		return errors.New("invalid_origin_scope")
	}
	if r.OriginScope.Kind == "conversation" {
		if r.ConversationKey == nil || *r.ConversationKey != r.OriginScope.Key {
			return errors.New("invalid_conversation_scope")
		}
	} else if r.ConversationKey != nil {
		return errors.New("unproven_conversation_association")
	}
	if !oneOf(r.Kind, "message", "tool_call", "tool_result", "title", "native_recap", "native_summary", "native_note", "compaction_marker", "lifecycle_observation", "opaque_record") {
		return errors.New("invalid_record_kind")
	}
	if r.SourceOrder != nil && !validOrder(r.SourceOrder) {
		return errors.New("invalid_source_order")
	}
	if (r.Role != nil && *r.Role == "") || (r.Channel != nil && *r.Channel == "") {
		return errors.New("empty_native_role_or_channel")
	}
	switch r.Coverage.Kind {
	case "unknown":
		if r.Coverage.Through != nil || r.Coverage.Start != nil || r.Coverage.End != nil {
			return errors.New("invalid_unknown_coverage")
		}
	case "collector_cutoff":
		if !validOrder(r.Coverage.Through) || r.Coverage.Start != nil || r.Coverage.End != nil {
			return errors.New("invalid_cutoff")
		}
	case "native_range":
		if !validOrder(r.Coverage.Start) || !validOrder(r.Coverage.End) || r.Coverage.Through != nil ||
			r.Coverage.Start.StreamGeneration != r.Coverage.End.StreamGeneration || r.Coverage.Start.Ordinal > r.Coverage.End.Ordinal {
			return errors.New("invalid_native_range")
		}
	default:
		return errors.New("invalid_coverage")
	}
	if !oneOf(r.Availability.State, "available", "partial", "unsupported", "opaque", "unavailable") ||
		(r.Availability.State != "available" && r.Availability.Reason == "") {
		return errors.New("invalid_availability")
	}
	if !oneOf(r.Provenance.Kind, "native", "user_annotation", "imported_external") || r.Provenance.Producer == "" {
		return errors.New("invalid_provenance")
	}
	if r.RawRef != nil && (r.RawRef.RecordKey != r.RecordKey || r.RawRef.SourceRevision != r.SourceRevision) {
		return errors.New("raw_reference_mismatch")
	}
	if r.Relations == nil {
		return errors.New("relations_must_be_array")
	}
	for _, rel := range r.Relations {
		if rel.Type == "" || rel.Target.Kind == "" || rel.Target.Key == "" || rel.Evidence == "" {
			return errors.New("invalid_relation")
		}
	}
	for _, p := range r.Body.Parts {
		if !oneOf(p.Type, "text", "tool_call", "tool_result", "opaque") {
			return errors.New("invalid_body_part")
		}
		if len(p.Data) != 0 && !json.Valid(p.Data) {
			return errors.New("invalid_part_data")
		}
	}
	return nil
}

// Decode rejects omitted required fields, unknown core fields and unsupported
// majors. Extensions are the sole forward-compatible metadata escape hatch.
func Decode(data []byte) (Record, error) {
	var r Record
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return r, errors.New("invalid_record_json")
	}
	for _, key := range []string{"contract_version", "source_namespace", "origin_scope", "conversation_key", "record_key", "source_revision", "provider", "provider_version", "adapter_version", "kind", "native_kind", "role", "channel", "source_time", "source_order", "observed_at", "body", "raw_ref", "coverage", "provenance", "relations", "availability"} {
		if _, ok := fields[key]; !ok {
			return r, fmt.Errorf("missing_field: %s", key)
		}
	}
	if err := rejectUnexpectedNull(data, reflect.TypeOf(r)); err != nil {
		return r, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return Record{}, errors.New("invalid_record_shape")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Record{}, errors.New("trailing_record_data")
	}
	return r, r.Validate()
}

// encoding/json normally accepts null for strings and structs. The public wire
// contract permits null only for explicitly nullable pointer fields and arbitrary
// extension/part data, so validate that distinction before decoding.
func rejectUnexpectedNull(data []byte, typ reflect.Type) error {
	if typ == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		if typ.Kind() == reflect.Pointer {
			return nil
		}
		return errors.New("unexpected_null")
	}
	if typ.Kind() == reflect.Pointer {
		return rejectUnexpectedNull(data, typ.Elem())
	}
	switch typ.Kind() {
	case reflect.Struct:
		if typ == reflect.TypeOf(time.Time{}) {
			return nil
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			return errors.New("invalid_record_shape")
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := field.Tag.Get("json")
			for j, c := range name {
				if c == ',' {
					name = name[:j]
					break
				}
			}
			if raw, ok := fields[name]; ok {
				if err := rejectUnexpectedNull(raw, field.Type); err != nil {
					return err
				}
			}
		}
	case reflect.Slice:
		var items []json.RawMessage
		if json.Unmarshal(data, &items) != nil {
			return errors.New("invalid_record_shape")
		}
		for _, item := range items {
			if err := rejectUnexpectedNull(item, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r Record) VerifyRaw(raw []byte) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if Digest(raw) != r.SourceRevision {
		return errors.New("source_digest_mismatch")
	}
	return nil
}
