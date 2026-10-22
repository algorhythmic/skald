package sessioncapture

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/algorhythmic/skald/sessionrecord"
)

// ClaudeSubagentAdapterVersion distinguishes the child conversation and relation
// semantics from the existing primary Claude adapter.
const ClaudeSubagentAdapterVersion = "claude-subagent-jsonl-v1.0.0"

const claudeSubagentPrefix = "sr1:claude-subagent:"

type claudeNativeIdentity struct {
	parent  string
	agent   string
	child   bool
	control bool
	known   bool
}

type claudeChildContext struct{ parent, agent string }

func needsClaudeForkProbe(input io.ReadSeeker) (bool, error) {
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	r := bufio.NewReaderSize(io.LimitReader(input, 4<<20), 64<<10)
	line, complete, oversized, err := readLine(r, 4<<20)
	if err != nil {
		return false, err
	}
	if oversized || !complete {
		return false, nil
	}
	var native object
	if json.Unmarshal(line, &native) != nil {
		return false, nil
	}
	return str(native, "type") == "fork-context-ref" && str(native, "agentId") != "" && str(native, "sessionId") == "", nil
}

func claudeControlKind(kind string) bool {
	switch kind {
	case "api-request", "api-request-blob", "api-request-shape", "fork-context-ref":
		return true
	}
	return false
}

// Claude writes the parent sessionId into sidechain lines. The native agentId
// and explicit true sidechain marker are required together: a path or filename
// never establishes a child identity.
func claudeIdentity(n object) (claudeNativeIdentity, error) {
	id := claudeNativeIdentity{parent: str(n, "sessionId")}
	marker, hasMarker := n["isSidechain"]
	var sidechain bool
	if hasMarker {
		var parsed *bool
		if json.Unmarshal(marker, &parsed) != nil || parsed == nil {
			return id, errors.New("subagent_identity_unsupported")
		}
		sidechain = *parsed
	}
	agentRaw, hasAgent := n["agentId"]
	if hasAgent && json.Unmarshal(agentRaw, &id.agent) != nil {
		return id, errors.New("subagent_identity_unsupported")
	}
	if sidechain {
		if id.agent == "" || id.parent == "" {
			return id, errors.New("subagent_identity_unsupported")
		}
		id.child, id.known = true, true
		return id, nil
	}
	if hasAgent {
		kind := str(n, "type")
		if hasMarker || id.agent == "" || !claudeControlKind(kind) || (id.parent == "" && kind != "fork-context-ref") {
			return id, errors.New("subagent_identity_unsupported")
		}
		id.control = true
		return id, nil
	}
	// A missing marker is normal for primary streams. Explicit false is also
	// primary evidence when the line has a native session ID.
	id.known = id.parent != "" || (hasMarker && !sidechain)
	return id, nil
}

// Child controls can precede the first sidechain message. Resolve their agent
// against a bounded native sidechain proof before normalizing any row.
func probeClaudeChild(input io.ReadSeeker, source Source) (claudeChildContext, error) {
	var ctx claudeChildContext
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return ctx, err
	}
	r := bufio.NewReaderSize(io.LimitReader(input, 4<<20), 64<<10)
	for i := 0; i < 256; i++ {
		line, complete, oversized, err := readLine(r, 4<<20)
		if err != nil {
			return ctx, err
		}
		if oversized {
			return ctx, errors.New("identity_probe_limit")
		}
		if !complete {
			break
		}
		var native object
		if json.Unmarshal(line, &native) != nil || native == nil {
			continue
		}
		id, err := claudeIdentity(native)
		if err != nil {
			return ctx, err
		}
		if id.child {
			if id.conversationID() != source.ConversationID {
				return ctx, errors.New("conversation_identity_mismatch")
			}
			return claudeChildContext{parent: id.parent, agent: id.agent}, nil
		}
	}
	return ctx, errors.New("conversation_identity_unavailable")
}

func (id claudeNativeIdentity) conversationID() string {
	if id.child || id.control {
		return sessionrecord.Key("claude-subagent", id.parent, id.agent)
	}
	return id.parent
}

func isClaudeSubagentConversation(id string) bool { return strings.HasPrefix(id, claudeSubagentPrefix) }

// IsClaudeSubagentConversation identifies the derived child ID format in saved
// registrations, including after a source has moved away from its original path.
func IsClaudeSubagentConversation(id string) bool { return isClaudeSubagentConversation(id) }
