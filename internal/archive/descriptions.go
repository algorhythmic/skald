package archive

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// DescriptionSelection is an archive-wide deterministic display projection.
// References remain exact even when display text is clipped. OrderingAmbiguous
// forbids interpreting the deterministic cross-stream selection as chronology.
type DescriptionSelection struct {
	Conversation      string            `json:"conversation_key"`
	Boundary          int64             `json:"boundary"`
	Kind              string            `json:"kind"`
	Selected          *TranscriptEntry  `json:"selected,omitempty"`
	PriorRecaps       []TranscriptEntry `json:"prior_recaps"`
	OrderingAmbiguous bool              `json:"ordering_ambiguous"`
	NewActivity       bool              `json:"new_activity"`
	RecapsTruncated   bool              `json:"recaps_truncated"`
}

// Descriptions inspects compact current metadata across the whole conversation,
// decoding at most six exact envelopes. It does not call a model, use arrival
// time as chronology, or expose system/developer content. Text is capped at 4KiB
// per item and prior recaps at four. A native generation is the order boundary.
func (s *Store) Descriptions(ctx context.Context, ns []string, conversation string) (DescriptionSelection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := DescriptionSelection{Conversation: conversation, Kind: "unavailable", PriorRecaps: []TranscriptEntry{}}
	in, args := scopeSQL(ns)
	args = append(args, conversation)
	var found int
	if e := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE namespace IN ("+in+") AND conversation_key=?", args...).Scan(&found); e != nil {
		return out, e
	}
	if found == 0 {
		return out, errors.New("not_found")
	}
	boundary, e := s.boundary()
	if e != nil {
		return out, e
	}
	out.Boundary = boundary
	// Windowing collapses replay observations to one position for each current
	// envelope. No compressed envelope is inflated by this metadata query.
	base := `WITH positions AS (SELECT n.rowid normalization_id,n.record_key,n.source_revision,n.adapter_version,n.kind,n.source_time,
 o.stream_key,o.generation,g.epoch,o.ordinal,
 row_number() OVER(PARTITION BY n.record_key,n.source_revision,n.adapter_version ORDER BY o.stream_key,g.epoch DESC,o.ordinal DESC) chosen
 FROM artifacts a JOIN record_heads h USING(record_key) JOIN normalization_storage n ON n.record_key=h.record_key AND n.source_revision=h.source_revision AND n.adapter_version=h.adapter_version
 JOIN record_observations o ON o.record_key=n.record_key AND o.source_revision=n.source_revision
 JOIN stream_generations g ON g.stream_key=o.stream_key AND g.generation=o.generation
 WHERE a.namespace IN (` + in + `) AND a.conversation_key=?) `
	var domains int
	if e = s.db.QueryRowContext(ctx, base+`SELECT count(DISTINCT stream_key||'/'||generation) FROM positions WHERE chosen=1`, args...).Scan(&domains); e != nil {
		return out, e
	}
	out.OrderingAmbiguous = domains > 1
	lookup := func(predicate string, limit int) ([]Artifact, error) {
		rows, e := s.db.QueryContext(ctx, base+`SELECT p.record_key,p.source_revision,p.adapter_version,p.kind,p.source_time,p.stream_key,p.generation,p.epoch,p.ordinal FROM positions p JOIN normalization_storage n ON n.rowid=p.normalization_id WHERE chosen=1 AND CASE WHEN p.kind IN ('native_recap','message') THEN coalesce(n.display_usable,trim(coalesce(json_extract(n.envelope_json,'$.body.text'),''))<>'' AND coalesce(json_extract(n.envelope_json,'$.availability.state'),'') IN ('available','partial') AND coalesce(json_extract(n.envelope_json,'$.body.state'),'') NOT IN ('unavailable','opaque','unsupported')) ELSE 0 END AND `+predicate+` ORDER BY stream_key DESC,epoch DESC,ordinal DESC,p.record_key DESC,p.source_revision DESC,p.adapter_version DESC LIMIT ?`, append(append([]any{}, args...), limit)...)
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		refs := []Artifact{}
		for rows.Next() {
			a := Artifact{Current: true}
			if e = rows.Scan(&a.Key, &a.Revision, &a.Adapter, &a.Kind, &a.SourceTime, &a.Stream, &a.Generation, &a.Epoch, &a.Ordinal); e != nil {
				return nil, e
			}
			refs = append(refs, a)
		}
		return refs, rows.Err()
	}
	recaps, e := lookup("p.kind='native_recap'", 6)
	if e != nil {
		return out, e
	}
	assistant, e := lookup("p.kind='message' AND coalesce(n.display_role,json_extract(n.envelope_json,'$.role'),'')='assistant'", 1)
	if e != nil {
		return out, e
	}
	var chosen *Artifact
	if len(recaps) > 0 {
		chosen = &recaps[0]
		out.Kind = "native_recap"
	}
	if len(assistant) > 0 && (chosen == nil || assistant[0].Stream == chosen.Stream && assistant[0].Generation == chosen.Generation && assistant[0].Ordinal > chosen.Ordinal) {
		chosen = &assistant[0]
		out.Kind = "assistant_excerpt"
	}
	project := func(a Artifact) (TranscriptEntry, error) {
		exact, e := s.get(ctx, ns, a.Key, a.Revision, a.Adapter, false)
		if e != nil {
			return TranscriptEntry{}, e
		}
		r := exact.Record
		if a.Kind == "" {
			a.Kind = r.Kind
			a.Current = true
		}
		text, clipped := displayClip(r.Body.Text, 4096)
		entry := TranscriptEntry{Artifact: a, Text: text, Truncated: clipped, NativeKind: r.NativeKind, Coverage: r.Coverage.Kind, State: r.Body.State, Availability: r.Availability}
		if r.Role != nil {
			entry.Role = *r.Role
		}
		return entry, nil
	}
	if chosen != nil {
		entry, e := project(*chosen)
		if e != nil {
			return out, e
		}
		out.Selected = &entry
	}
	for _, a := range recaps {
		if chosen != nil && a.Key == chosen.Key && a.Revision == chosen.Revision && a.Adapter == chosen.Adapter {
			continue
		}
		if len(out.PriorRecaps) == 4 {
			out.RecapsTruncated = true
			break
		}
		entry, e := project(a)
		if e != nil {
			return out, e
		}
		out.PriorRecaps = append(out.PriorRecaps, entry)
	}
	if len(recaps) > 0 {
		r := recaps[0]
		activityArgs := append(append([]any{}, args...), r.Stream, r.Generation, r.Ordinal)
		if e = s.db.QueryRowContext(ctx, base+`SELECT EXISTS(SELECT 1 FROM positions WHERE chosen=1 AND stream_key=? AND generation=? AND ordinal>? AND kind IN ('message','tool_call','tool_result'))`, activityArgs...).Scan(&out.NewActivity); e != nil {
			return out, e
		}
	}
	if out.Selected == nil {
		var a Artifact
		var title string
		var ambiguous bool
		e = s.db.QueryRowContext(ctx, `SELECT record_key,source_revision,adapter_version,title,ordering_ambiguous FROM session_titles WHERE conversation_key=? AND origin='native'`, conversation).Scan(&a.Key, &a.Revision, &a.Adapter, &title, &ambiguous)
		if e != nil && e != sql.ErrNoRows {
			return out, e
		}
		if e == nil && strings.TrimSpace(title) != "" {
			entry, e := project(a)
			if e != nil {
				return out, e
			}
			entry.Text, entry.Truncated = displayClip(title, 4096)
			out.Selected = &entry
			out.Kind = "native_title"
			out.OrderingAmbiguous = out.OrderingAmbiguous || ambiguous
		}
	}
	return out, nil
}
