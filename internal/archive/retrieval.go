package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/algorhythmic/skald/internal/secretredact"
	"github.com/algorhythmic/skald/sessionrecord"
)

// RetrievalRecord is an exact normalized citation, never an original transcript.
type RetrievalRecord struct {
	ID        string            `json:"id"`
	Ref       sessionrecord.Ref `json:"ref"`
	Adapter   string            `json:"adapter_version"`
	Namespace string            `json:"namespace"`
	Role      string            `json:"role"`
	Text      string            `json:"text"`
	Timestamp string            `json:"timestamp"`
}
type RetrievalSnapshot struct {
	Instance        string            `json:"archive_instance"`
	CorpusRevision  int64             `json:"corpus_revision"`
	Boundary        int64             `json:"boundary"`
	ScopeGeneration int64             `json:"scope_generation"`
	Records         []RetrievalRecord `json:"records"`
}

func (s *Store) retrievalScope(ctx context.Context, ns []string, generation int64) error {
	var current int64
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT generation,namespaces_json FROM archive_scope WHERE singleton=1").Scan(&current, &raw); err != nil {
		return err
	}
	var allowed []string
	if json.Unmarshal([]byte(raw), &allowed) != nil || current != generation || len(ns) == 0 {
		return errors.New("retrieval_scope_changed")
	}
	seen := map[string]bool{}
	for _, n := range ns {
		if n == "" || seen[n] || !slices.Contains(allowed, n) {
			return errors.New("retrieval_scope_changed")
		}
		seen[n] = true
	}
	return nil
}

// RetrievalSnapshot materializes permitted current-head user/assistant messages.
// maxBytes bounds retained projected JSON, not discarded full native envelopes.
// A full normalized envelope is decoded once, with a separate 64 MiB row limit.
// Over-limit corpora fail instead of silently truncating.
func (s *Store) RetrievalSnapshot(ctx context.Context, ns []string, generation int64, maxRecords, maxBytes int) (RetrievalSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := RetrievalSnapshot{Instance: s.instance, ScopeGeneration: generation, Records: []RetrievalRecord{}}
	if maxRecords < 1 || maxRecords > 250000 || maxBytes < 1 || maxBytes > 256<<20 {
		return out, errors.New("invalid_retrieval_snapshot_limit")
	}
	if err := s.retrievalScope(ctx, ns, generation); err != nil {
		return out, err
	}
	var err error
	out.Boundary, err = s.boundary()
	if err != nil {
		return out, err
	}
	if out.CorpusRevision, err = s.retrievalRevision(ctx, ns); err != nil {
		return out, err
	}
	in, args := scopeSQL(ns)
	logical := `CASE WHEN n.decoded_length>=0 THEN n.decoded_length ELSE length(CAST(n.envelope_json AS BLOB)) END`
	rows, err := s.db.QueryContext(ctx, `SELECT `+logical+`,CASE WHEN `+logical+`<=? THEN n.envelope_json ELSE '' END,CASE WHEN `+logical+`<=? THEN n.compressed_envelope ELSE NULL END,n.decoded_length,coalesce(n.envelope_digest,'') FROM normalization_storage n JOIN record_heads h ON h.record_key=n.record_key AND h.source_revision=n.source_revision AND h.adapter_version=n.adapter_version JOIN artifacts a ON a.record_key=n.record_key WHERE a.namespace IN (`+in+`) AND n.kind='message' AND coalesce(n.display_role,json_extract(n.envelope_json,'$.role')) IN ('user','assistant') AND coalesce(n.display_usable,(trim(coalesce(json_extract(n.envelope_json,'$.body.text'),''))<>'' AND coalesce(json_extract(n.envelope_json,'$.availability.state'),'') IN ('available','partial') AND coalesce(json_extract(n.envelope_json,'$.body.state'),'') NOT IN ('unavailable','opaque','unsupported')))=1 ORDER BY n.record_key,n.source_revision,n.adapter_version LIMIT ?`, append(append([]any{maxPayloadBytes, maxPayloadBytes}, args...), maxRecords+1)...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	used := 2
	scanned := 0
	for rows.Next() {
		var raw string
		var rawBytes int64
		var packed []byte
		var decoded sql.NullInt64
		var digest string
		if err := rows.Scan(&rawBytes, &raw, &packed, &decoded, &digest); err != nil {
			return out, err
		}
		scanned++
		if rawBytes > maxPayloadBytes || scanned > maxRecords {
			return RetrievalSnapshot{}, errors.New("retrieval_snapshot_too_large")
		}
		var envelope []byte
		if packed != nil {
			envelope, err = inflatePayload(packed, decoded.Int64)
			if err != nil {
				return out, err
			}
			if sessionrecord.Digest(envelope) != digest {
				return out, errors.New("payload_digest_mismatch")
			}
		} else {
			envelope = []byte(raw)
		}
		if secretredact.Check(envelope) != nil {
			return RetrievalSnapshot{}, secretredact.ErrSensitive
		}
		rec, err := sessionrecord.Decode(envelope)
		if err != nil {
			return out, err
		}
		if rec.Role == nil || (*rec.Role != "user" && *rec.Role != "assistant") || rec.Kind != "message" {
			return out, errors.New("invalid_retrieval_projection")
		}
		if rec.Body.Text == "" {
			continue
		}
		projected := RetrievalRecord{ID: "r:" + strconv.Itoa(scanned), Ref: sessionrecord.Ref{RecordKey: rec.RecordKey, SourceRevision: rec.SourceRevision}, Adapter: rec.AdapterVersion, Namespace: rec.SourceNamespace, Role: *rec.Role, Text: rec.Body.Text, Timestamp: rec.ObservedAt.UTC().Format(time.RFC3339Nano)}
		encoded, _ := json.Marshal(projected)
		used += len(encoded) + 1
		if used > maxBytes {
			return RetrievalSnapshot{}, errors.New("retrieval_snapshot_too_large")
		}
		out.Records = append(out.Records, projected)
	}
	return out, rows.Err()
}

// ValidateRetrieval checks live permissions, scoped withdrawals and cited heads,
// then exact-reads every citation under one lock. Appends and unrelated namespace
// changes preserve the validity of evidence already captured in the snapshot.
func (s *Store) ValidateRetrieval(ctx context.Context, ns []string, snapshot RetrievalSnapshot, records []RetrievalRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snapshot.Instance != s.instance {
		return errors.New("retrieval_archive_changed")
	}
	if err := s.retrievalScope(ctx, ns, snapshot.ScopeGeneration); err != nil {
		return err
	}
	in, args := scopeSQL(ns)
	var withdrawn int
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM changes INDEXED BY changes_namespace_sequence WHERE namespace IN ("+in+") AND change_kind='withdrawal' AND sequence>?)", append(args, snapshot.Boundary)...).Scan(&withdrawn); err != nil {
		return err
	}
	if withdrawn != 0 {
		return errors.New("retrieval_archive_changed")
	}
	for _, r := range records {
		var current int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM record_heads WHERE record_key=? AND source_revision=? AND adapter_version=?", r.Ref.RecordKey, r.Ref.SourceRevision, r.Adapter).Scan(&current); err != nil {
			return err
		}
		if current != 1 {
			return errors.New("retrieval_projection_changed")
		}
		got, err := s.get(ctx, ns, r.Ref.RecordKey, r.Ref.SourceRevision, r.Adapter, false)
		if err != nil {
			return err
		}
		if got.Record.Body.Text != r.Text || got.Record.Role == nil || *got.Record.Role != r.Role || got.Record.Kind != "message" {
			return errors.New("retrieval_projection_changed")
		}
	}
	return nil
}

// RetrievalIdentity performs only authorization and change-boundary checks; it
// lets a warm retrieval publication avoid decompressing its unchanged corpus.
func (s *Store) RetrievalIdentity(ctx context.Context, ns []string, generation int64) (RetrievalSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := RetrievalSnapshot{Instance: s.instance, ScopeGeneration: generation}
	if err := s.retrievalScope(ctx, ns, generation); err != nil {
		return out, err
	}
	var err error
	out.Boundary, err = s.boundary()
	if err == nil {
		out.CorpusRevision, err = s.retrievalRevision(ctx, ns)
	}
	return out, err
}

func (s *Store) retrievalRevision(ctx context.Context, ns []string) (int64, error) {
	in, args := scopeSQL(ns)
	var revision int64
	err := s.db.QueryRowContext(ctx, "SELECT coalesce(max(sequence),0) FROM changes INDEXED BY changes_namespace_sequence WHERE namespace IN ("+in+") AND change_kind IN ('record','withdrawal')", args...).Scan(&revision)
	return revision, err
}
