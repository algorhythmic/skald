package sessioncapture

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"skald/sessionrecord"
)

type object map[string]json.RawMessage

func str(m object, key string) string { var s string; _ = json.Unmarshal(m[key], &s); return s }
func obj(m object, key string) object { var o object; _ = json.Unmarshal(m[key], &o); return o }
func textPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func normalize(raw []byte, source Source, cp *Checkpoint, now time.Time) (sessionrecord.Record, string, error) {
	var native object
	malformed := !utf8.Valid(raw) || json.Unmarshal(raw, &native) != nil || native == nil
	id, nativeID, nativeKind := "", "", "unknown"
	if !malformed {
		nativeKind = str(native, "type")
		if nativeKind == "" {
			nativeKind = "unknown"
		}
		if source.Provider == Claude {
			id, nativeID = str(native, "sessionId"), str(native, "uuid")
			if v := str(native, "version"); v != "" {
				cp.ProviderVersion = v
			}
			if sub := str(native, "subtype"); sub != "" {
				nativeKind += "/" + sub
			}
		} else if nativeKind == "session_meta" {
			p := obj(native, "payload")
			id = str(p, "id")
			if v := str(p, "cli_version"); v != "" {
				cp.ProviderVersion = v
			}
		}
	}
	if id != "" {
		if cp.ConversationID != "" && cp.ConversationID != id {
			return sessionrecord.Record{}, "", errors.New("conversation_identity_mismatch")
		}
		cp.ConversationID = id
	}
	if cp.ConversationID == "" {
		return sessionrecord.Record{}, "", errors.New("conversation_identity_unavailable")
	}
	conversation := sessionrecord.Conversation(source.Namespace, cp.ConversationID)
	scope := sessionrecord.Scope{Kind: "conversation", Key: conversation}
	key := sessionrecord.OrdinalRecord(scope, cp.Generation, cp.Ordinal)
	if nativeID != "" {
		key = sessionrecord.NativeRecord(scope, nativeID)
	}
	r := sessionrecord.Record{
		ContractVersion: 1, SourceNamespace: source.Namespace, OriginScope: scope,
		ConversationKey: &conversation, RecordKey: key, SourceRevision: sessionrecord.Digest(raw),
		Provider: source.Provider, ProviderVersion: cp.ProviderVersion, AdapterVersion: AdapterVersion,
		Kind: "opaque_record", NativeKind: nativeKind, ObservedAt: now.UTC(),
		SourceOrder: &sessionrecord.Order{StreamGeneration: cp.Generation, Ordinal: cp.Ordinal},
		Coverage:    sessionrecord.Coverage{Kind: "unknown"}, Provenance: sessionrecord.Provenance{Kind: "native", Producer: source.Provider},
		Relations: []sessionrecord.Relation{}, Availability: sessionrecord.Availability{State: "available"},
	}
	r.RawRef = &sessionrecord.Ref{RecordKey: key, SourceRevision: r.SourceRevision}
	if malformed {
		r.Availability = sessionrecord.Availability{State: "opaque", Reason: "malformed_json"}
		return r, "malformed_json", nil
	}
	code := ""
	if stamp := str(native, "timestamp"); stamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			r.SourceTime = &t
		} else {
			code = "invalid_source_time"
		}
	}
	if source.Provider == Claude {
		normalizeClaude(native, &r)
	} else {
		normalizeCodex(native, &r)
	}
	if r.Kind == "opaque_record" {
		r.Availability = sessionrecord.Availability{State: "opaque", Reason: "unsupported_native_kind"}
	}
	if code != "" {
		r.Availability = sessionrecord.Availability{State: "partial", Reason: code}
	}
	return r, code, nil
}

func normalizeClaude(n object, r *sessionrecord.Record) {
	switch r.NativeKind {
	case "user", "assistant":
		m := obj(n, "message")
		if m == nil {
			return
		}
		r.Kind = "message"
		r.Role = textPtr(str(m, "role"))
		r.Body = content(m["content"])
		if r.Body.Text == "" && len(r.Body.Parts) == 0 {
			r.Kind = "opaque_record"
		}
	case "system/away_summary":
		if s := str(n, "content"); s != "" {
			r.Kind = "native_recap"
			r.Body.Text = s
			r.Role = textPtr("system")
		}
	case "ai-title":
		if s := str(n, "title"); s != "" {
			r.Kind = "title"
			r.Body.Text = s
		}
	}
	if cwd := str(n, "cwd"); cwd != "" {
		r.Extensions = map[string]json.RawMessage{"native_cwd": n["cwd"]}
	}
}

func normalizeCodex(n object, r *sessionrecord.Record) {
	p := obj(n, "payload")
	switch r.NativeKind {
	case "session_meta":
		// Metadata proves identity and cwd, not a live session-start observation.
		r.Extensions = map[string]json.RawMessage{}
		for _, k := range []string{"cwd", "originator", "source"} {
			if v, ok := p[k]; ok {
				r.Extensions["native_"+k] = v
			}
		}
	case "response_item":
		t := str(p, "type")
		r.NativeKind += "/" + t
		r.Role, r.Channel = textPtr(str(p, "role")), textPtr(str(p, "channel"))
		switch t {
		case "message":
			r.Kind, r.Body = "message", content(p["content"])
			if r.Body.Text == "" && len(r.Body.Parts) == 0 {
				r.Kind = "opaque_record"
			}
		case "function_call", "custom_tool_call":
			r.Kind = "tool_call"
			r.Body.Parts = []sessionrecord.Part{{Type: "tool_call", ID: str(p, "call_id"), Name: str(p, "name"), Data: n["payload"]}}
		case "function_call_output", "custom_tool_call_output":
			r.Kind = "tool_result"
			r.Body.Parts = []sessionrecord.Part{{Type: "tool_result", ID: str(p, "call_id"), Data: n["payload"]}}
		}
	case "compacted":
		r.Kind = "compaction_marker"
		r.Body.Text = str(p, "message")
		if r.Body.Text == "" {
			r.Availability = sessionrecord.Availability{State: "opaque", Reason: "compaction_text_unavailable"}
		}
	case "event_msg":
		t := str(p, "type")
		r.NativeKind += "/" + t
		// A completed turn is idle evidence; it is not a conversation end.
		switch t {
		case "task_started":
			r.Kind = "lifecycle_observation"
			r.Body.State = "working"
		case "task_complete":
			r.Kind = "lifecycle_observation"
			r.Body.State = "idle"
		}
		// agent_message/user_message mirrors are preserved opaque; response_item
		// is the ordinary-content stream, avoiding duplicate displayed utterances.
	}
}

func content(raw json.RawMessage) sessionrecord.Body {
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return sessionrecord.Body{Text: s}
	}
	var values []object
	if json.Unmarshal(raw, &values) != nil {
		return sessionrecord.Body{}
	}
	b := sessionrecord.Body{}
	var texts []string
	for _, v := range values {
		t := str(v, "type")
		p := sessionrecord.Part{Type: "opaque"}
		switch t {
		case "text", "input_text", "output_text":
			p.Type, p.Text = "text", str(v, "text")
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		case "tool_use":
			p.Type, p.ID, p.Name = "tool_call", str(v, "id"), str(v, "name")
		case "tool_result":
			p.Type, p.ID = "tool_result", str(v, "tool_use_id")
		}
		if p.Type != "text" {
			p.Data, _ = json.Marshal(v)
		}
		b.Parts = append(b.Parts, p)
	}
	b.Text = strings.Join(texts, "\n\n")
	return b
}
