package sessioncapture

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Fork metadata can precede copied ancestor headers and history. Only an
// explicit, ordered forked_from_id chain permits distinct native metadata IDs.
// The chain is bounded independently of the number/size of inherited messages.
const (
	CodexForkAdapterVersion = "codex-fork-jsonl-v1.0.0"
	maxCodexForkAncestors   = 16
)

type codexMetadata struct{ id, parent string }

type codexForkContext struct {
	root    string
	parents map[string]string
	tail    string
}

func codexOptionalID(p object, key string) (string, error) {
	raw, ok := p[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var id string
	if json.Unmarshal(raw, &id) != nil || id == "" || len(id) > 256 {
		return "", errors.New("conversation_identity_mismatch")
	}
	return id, nil
}

func codexSessionMetadata(n object) (codexMetadata, error) {
	p := obj(n, "payload")
	m := codexMetadata{id: str(p, "id")}
	if len(m.id) > 256 {
		return m, errors.New("identity_metadata_too_large")
	}
	var err error
	m.parent, err = codexOptionalID(p, "forked_from_id")
	if err != nil {
		return m, err
	}
	// A thread spawn corroborates the fork edge. A source string, session_id,
	// filename, or cwd alone never authorizes conflicting session_meta IDs.
	subagent := obj(obj(p, "source"), "subagent")
	if raw, ok := subagent["thread_spawn"]; ok {
		var spawn object
		if json.Unmarshal(raw, &spawn) != nil || spawn == nil {
			return m, errors.New("conversation_identity_mismatch")
		}
		parent, err := codexOptionalID(spawn, "parent_thread_id")
		if err != nil || parent == "" || (m.parent != "" && parent != m.parent) {
			return m, errors.New("conversation_identity_mismatch")
		}
	}
	if m.parent != "" && (m.id == "" || m.parent == m.id) {
		return m, errors.New("conversation_identity_mismatch")
	}
	return m, nil
}

func (c *codexForkContext) observe(m codexMetadata) error {
	if m.id == "" {
		return nil
	}
	if len(m.id) > 256 {
		return errors.New("identity_metadata_too_large")
	}
	if c.root == "" {
		c.root, c.tail = m.id, m.parent
		c.parents = map[string]string{m.id: m.parent}
		return nil
	}
	if parent, ok := c.parents[m.id]; ok {
		if parent != m.parent {
			return errors.New("conversation_identity_mismatch")
		}
		return nil
	}
	if m.id != c.tail {
		return errors.New("conversation_identity_mismatch")
	}
	if m.parent != "" && len(c.parents) >= maxCodexForkAncestors {
		return errors.New("identity_probe_limit")
	}
	if m.parent != "" {
		if _, cycle := c.parents[m.parent]; cycle || m.parent == m.id {
			return errors.New("conversation_identity_mismatch")
		}
	}
	c.parents[m.id], c.tail = m.parent, m.parent
	return nil
}

// validate never expands lineage: only the bounded native prefix can establish
// ancestor edges. A directly named terminal parent may appear after the prefix.
func (c codexForkContext) validate(m codexMetadata, conversationID string) error {
	if m.id == "" {
		return nil
	}
	if c.root == "" {
		if m.parent != "" || (conversationID != "" && conversationID != m.id) {
			return errors.New("conversation_identity_mismatch")
		}
		return nil
	}
	if conversationID != "" && conversationID != c.root {
		return errors.New("conversation_identity_mismatch")
	}
	if parent, ok := c.parents[m.id]; ok && parent == m.parent {
		return nil
	}
	if m.id == c.tail && m.parent == "" {
		return nil
	}
	return errors.New("conversation_identity_mismatch")
}

// Replay only the bounded identity prefix, preserving the checkpoint wire shape.
// Ordinary streams stop at their first metadata; forks stop at the terminal
// ancestor metadata, usually the immediately following copied parent header.
// Invalid metadata is left for normal ingestion to block at its exact offset.
func probeCodexFork(input io.ReadSeeker) (codexForkContext, error) {
	var ctx codexForkContext
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return ctx, err
	}
	r := bufio.NewReaderSize(io.LimitReader(input, 4<<20), 64<<10)
	for i := 0; i < 256; i++ {
		line, complete, oversized, err := readLine(r, 4<<20)
		if err != nil {
			return ctx, err
		}
		if oversized || !complete {
			break
		}
		var n object
		if json.Unmarshal(line, &n) != nil || n == nil || str(n, "type") != "session_meta" {
			continue
		}
		m, err := codexSessionMetadata(n)
		if err != nil || ctx.observe(m) != nil {
			break
		}
		if ctx.root != "" && ctx.tail == "" {
			break
		}
	}
	return ctx, nil
}
