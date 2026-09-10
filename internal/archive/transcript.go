package archive

import (
	"context"
	"errors"
	"unicode/utf8"

	"skald/sessionrecord"
)

// TranscriptEntry is a display projection, never an exact record envelope. Exact
// references survive clipping, and raw originals stay behind the exact-read API.
type TranscriptEntry struct {
	Artifact
	Role         string                     `json:"role"`
	Text         string                     `json:"text"`
	NativeKind   string                     `json:"native_kind"`
	Coverage     string                     `json:"coverage"`
	State        string                     `json:"state"`
	Availability sessionrecord.Availability `json:"availability"`
	Parts        []DisplayPart              `json:"parts"`
	Truncated    bool                       `json:"truncated"`
}
type DisplayPart struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Text string `json:"text"`
}

// Transcript returns newest-first pages, ordered within native streams, under
// one archive lock/boundary. Cross-stream order is deterministic, not causal.
// 25 entries, 16 KiB text + 16 KiB parts each bound display memory and responses.
func (s *Store) Transcript(ctx context.Context, ns []string, conversation, cursor string, limit int, history bool) (Page[TranscriptEntry], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 1 || limit > 25 {
		return Page[TranscriptEntry]{}, errors.New("invalid_page_limit")
	}
	refs, err := s.artifacts(ctx, ns, conversation, cursor, limit, true, history)
	out := Page[TranscriptEntry]{Items: []TranscriptEntry{}, Boundary: refs.Boundary, Next: refs.Next}
	if err != nil {
		return out, err
	}
	for _, ref := range refs.Items {
		exact, err := s.get(ctx, ns, ref.Key, ref.Revision, ref.Adapter, false)
		if err != nil {
			return out, err
		}
		r := exact.Record
		e := TranscriptEntry{Artifact: ref, NativeKind: r.NativeKind, Coverage: r.Coverage.Kind, State: r.Body.State, Availability: r.Availability}
		if r.Role != nil {
			e.Role = *r.Role
		}
		e.Text, e.Truncated = displayClip(r.Body.Text, 16<<10)
		remaining := 16 << 10
		for _, p := range r.Body.Parts {
			if p.Type == "text" || p.Type == "input_text" || p.Type == "output_text" {
				continue
			}
			if len(e.Parts) == 16 || remaining == 0 {
				e.Truncated = true
				break
			}
			value := p.Text
			if len(p.Data) > 0 {
				value = string(p.Data)
			}
			value, clipped := displayClip(value, remaining)
			remaining -= len(value)
			name, nameClipped := displayClip(p.Name, 128)
			typ, typeClipped := displayClip(p.Type, 128)
			e.Truncated = e.Truncated || clipped || nameClipped || typeClipped
			e.Parts = append(e.Parts, DisplayPart{Type: typ, Name: name, Text: value})
		}
		out.Items = append(out.Items, e)
	}
	return out, nil
}
func displayClip(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
