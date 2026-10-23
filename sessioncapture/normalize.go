package sessioncapture

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/algorhythmic/skald/sessionrecord"
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

func normalize(raw []byte, source Source, cp *Checkpoint, now time.Time, childContext claudeChildContext, codexContext codexForkContext) (sessionrecord.Record, string, error) {
	var native object
	malformed := !utf8.Valid(raw) || json.Unmarshal(raw, &native) != nil || native == nil
	id, nativeID, nativeKind := "", "", "unknown"
	var claudeID claudeNativeIdentity
	var codexMeta codexMetadata
	registeredChild := source.Provider == Claude && isClaudeSubagentConversation(source.ConversationID)
	if !malformed {
		nativeKind = str(native, "type")
		if nativeKind == "" {
			nativeKind = "unknown"
		}
		if source.Provider == Claude {
			var err error
			claudeID, err = claudeIdentity(native)
			if err != nil {
				return sessionrecord.Record{}, "", err
			}
			if claudeID.child {
				if !registeredChild || source.ConversationID != claudeID.conversationID() {
					return sessionrecord.Record{}, "", errors.New("subagent_identity_unsupported")
				}
				if childContext.parent != "" && (claudeID.parent != childContext.parent || claudeID.agent != childContext.agent) {
					return sessionrecord.Record{}, "", errors.New("conversation_identity_mismatch")
				}
				childContext = claudeChildContext{parent: claudeID.parent, agent: claudeID.agent}
			} else if claudeID.control {
				if !registeredChild {
					return sessionrecord.Record{}, "", errors.New("subagent_identity_unsupported")
				}
				if claudeID.parent != "" {
					if claudeID.conversationID() != source.ConversationID {
						return sessionrecord.Record{}, "", errors.New("conversation_identity_mismatch")
					}
					childContext = claudeChildContext{parent: claudeID.parent, agent: claudeID.agent}
				}
				if childContext.parent == "" || claudeID.agent != childContext.agent || (claudeID.parent != "" && claudeID.parent != childContext.parent) {
					return sessionrecord.Record{}, "", errors.New("conversation_identity_mismatch")
				}
			} else if registeredChild && claudeID.known {
				return sessionrecord.Record{}, "", errors.New("conversation_identity_mismatch")
			}
			id, nativeID = claudeID.conversationID(), str(native, "uuid")
			if claudeID.control {
				id = source.ConversationID
			}
			if v := str(native, "version"); v != "" {
				cp.ProviderVersion = v
			}
			if sub := str(native, "subtype"); sub != "" {
				nativeKind += "/" + sub
			}
		} else if source.Provider == Devin {
			// Canonical store lines carry kind/position/payload; identity is the
			// database's native session UUID established at enrollment.
			nativeKind = str(native, "kind")
			if nativeKind == "" {
				nativeKind = "unknown"
			}
			id = source.ConversationID
		} else if nativeKind == "session_meta" {
			p := obj(native, "payload")
			var err error
			codexMeta, err = codexSessionMetadata(native)
			if err != nil {
				return sessionrecord.Record{}, "", err
			}
			if err := codexContext.validate(codexMeta, cp.ConversationID); err != nil {
				return sessionrecord.Record{}, "", err
			}
			id = codexMeta.id
			if codexContext.root != "" && id != "" {
				id = codexContext.root
			}
			if codexMeta.id == id {
				if v := str(p, "cli_version"); v != "" {
					cp.ProviderVersion = v
				}
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
	if registeredChild {
		r.AdapterVersion = ClaudeSubagentAdapterVersion
	}
	if source.Provider == Codex && codexContext.parents[codexContext.root] != "" {
		r.AdapterVersion = CodexForkAdapterVersion
	}
	if source.Provider == Codex && codexContext.root == cp.ConversationID && codexContext.parents[codexContext.root] != "" {
		parent := sessionrecord.Conversation(source.Namespace, codexContext.parents[codexContext.root])
		evidence := "bounded_native_forked_from_id_prefix"
		if codexMeta.id == cp.ConversationID && codexMeta.parent != "" {
			evidence = "native_forked_from_id"
		}
		r.Relations = append(r.Relations, sessionrecord.Relation{Type: "native_fork_of", Target: sessionrecord.Scope{Kind: "conversation", Key: parent}, Evidence: evidence})
	}
	if claudeID.child || claudeID.control {
		parent := sessionrecord.Conversation(source.Namespace, childContext.parent)
		evidence := "native_sessionId_agentId_isSidechain"
		if claudeID.control {
			evidence = "native_agentId_verified_by_sidechain"
		}
		r.Relations = append(r.Relations, sessionrecord.Relation{Type: "native_subagent_of", Target: sessionrecord.Scope{Kind: "conversation", Key: parent}, Evidence: evidence})
		r.Extensions = map[string]json.RawMessage{
			"native_agent_id": native["agentId"],
		}
		if v, ok := native["sessionId"]; ok {
			r.Extensions["native_parent_session_id"] = v
		}
		if v, ok := native["isSidechain"]; ok {
			r.Extensions["native_is_sidechain"] = v
		}
		if parentUUID, ok := native["parentUuid"]; ok {
			r.Extensions["native_parent_uuid"] = parentUUID
		}
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
	} else if source.Provider == Devin {
		normalizeDevin(native, &r)
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
	case "system/turn_duration":
		// Written when a turn finishes; it is not a conversation end.
		r.Kind = "lifecycle_observation"
		r.Body.State = "idle"
	case "ai-title":
		s := str(n, "aiTitle")
		if s == "" {
			s = str(n, "title")
		}
		if s != "" {
			r.Kind = "title"
			r.Body.Text = s
		}
	}
	if cwd := str(n, "cwd"); cwd != "" {
		if r.Extensions == nil {
			r.Extensions = map[string]json.RawMessage{}
		}
		r.Extensions["native_cwd"] = n["cwd"]
	}
}

func normalizeCodex(n object, r *sessionrecord.Record) {
	p := obj(n, "payload")
	switch r.NativeKind {
	case "session_meta":
		// Metadata proves identity and cwd, not a live session-start observation.
		// A distinct session_id is the parent thread of a compaction-resume file.
		r.Extensions = map[string]json.RawMessage{}
		prefix := "native_"
		fork := r.AdapterVersion == CodexForkAdapterVersion
		if fork && r.ConversationKey != nil && *r.ConversationKey != sessionrecord.Conversation(r.SourceNamespace, str(p, "id")) {
			// Copied metadata is evidence about its original ancestor, not
			// project attribution or continuation-title evidence for this child.
			prefix = "native_inherited_"
		}
		for _, k := range []string{"cwd", "originator", "source"} {
			if v, ok := p[k]; ok {
				r.Extensions[prefix+k] = v
			}
		}
		if fork {
			if v, ok := p["id"]; ok {
				r.Extensions["native_session_id"] = v
			}
			if v, ok := p["forked_from_id"]; ok {
				r.Extensions["native_forked_from_id"] = v
			}
		}
		if parent := str(p, "session_id"); parent != "" && parent != str(p, "id") {
			if v, err := json.Marshal(parent); err == nil {
				r.Extensions[prefix+"parent"] = v
			}
		}
	case "response_item":
		t := str(p, "type")
		r.NativeKind += "/" + t
		r.Role, r.Channel = textPtr(str(p, "role")), textPtr(str(p, "channel"))
		switch t {
		case "compaction":
			// Compaction payloads are encrypted; the marker keeps the event
			// visible without pretending its content was captured.
			r.Kind = "compaction_marker"
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
