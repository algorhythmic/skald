package archive

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

type Cursor struct {
	Instance string
	Scope    string
	Boundary int64
	After    string
}
type Page[T any] struct {
	Items    []T    `json:"items"`
	Boundary int64  `json:"boundary"`
	Next     string `json:"next_cursor,omitempty"`
}
type Project struct {
	Path string            `json:"path"`
	Ref  sessionrecord.Ref `json:"ref"`
}
type Session struct {
	Projects               []Project          `json:"projects"`
	ProjectsTruncated      bool               `json:"projects_truncated"`
	Key                    string             `json:"conversation_key"`
	Namespace              string             `json:"namespace"`
	Provider               string             `json:"provider"`
	NativeID               string             `json:"native_id"`
	IsSubagent             bool               `json:"is_subagent"`
	Title                  string             `json:"title"`
	TitleRef               *sessionrecord.Ref `json:"title_ref,omitempty"`
	TitleKind              string             `json:"title_kind,omitempty"`
	TitleOrderingAmbiguous bool               `json:"title_ordering_ambiguous"`
	RecordVersions         int64              `json:"record_versions"`
	SourceHealth           string             `json:"source_health"`
	Activity               string             `json:"activity"`
	ActivityAmbiguous      bool               `json:"activity_ordering_ambiguous"`
	LastRecordTime         *string            `json:"last_record_time,omitempty"`
}
type Artifact struct {
	Key        string  `json:"record_key"`
	Revision   string  `json:"source_revision"`
	Adapter    string  `json:"adapter_version"`
	Kind       string  `json:"kind"`
	SourceTime *string `json:"source_time"`
	Current    bool    `json:"current"`
	Stream     string  `json:"stream_key"`
	Generation string  `json:"stream_generation"`
	Epoch      int64   `json:"stream_epoch"`
	Ordinal    int64   `json:"ordinal"`
}
type Exact struct {
	Record   sessionrecord.Record `json:"record"`
	Raw      []byte               `json:"raw,omitempty"`
	Boundary int64                `json:"boundary"`
}
type SourceHealth struct {
	Key                 string  `json:"stream_key"`
	Namespace           string  `json:"namespace"`
	Provider            string  `json:"provider"`
	Enrolled            bool    `json:"enrolled"`
	Health              string  `json:"health"`
	CheckedAt           *string `json:"checked_at"`
	CheckpointAvailable bool    `json:"checkpoint_available"`
	Generation          *string `json:"stream_generation"`
	Epoch               *int64  `json:"stream_epoch"`
	NextOrdinal         *int64  `json:"next_ordinal"`
	CapturedOffset      int64   `json:"captured_offset"`
	ParsedOffset        int64   `json:"parsed_offset"`
	HasGaps             bool    `json:"has_gaps"`
	// These describe the record immediately before the committed checkpoint in
	// its current generation. Checks and empty polls never refresh record age.
	// A missing native timestamp is unknown, even if an earlier record had one.
	LastCapturedAt *string `json:"last_captured_at"`
	LastSourceTime *string `json:"last_source_time"`
}

func (s *Store) cursor(namespaces []string, kind, encoded string, limit int) (Cursor, error) {
	if limit < 1 || limit > 100 {
		return Cursor{}, errors.New("invalid_page_limit")
	}
	ns := append([]string{}, namespaces...)
	sort.Strings(ns)
	scope := sessionrecord.Key("read-scope", append([]string{kind}, ns...)...)
	boundary, err := s.boundary()
	if err != nil {
		return Cursor{}, err
	}
	c := Cursor{Instance: s.instance, Scope: scope, Boundary: boundary}
	if encoded != "" {
		if len(encoded) > 4096 {
			return c, errors.New("invalid_cursor")
		}
		raw, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return c, errors.New("invalid_cursor")
		}
		if json.Unmarshal(raw, &c) != nil {
			return c, errors.New("invalid_cursor")
		}
		if c.Instance != s.instance || c.Scope != scope || c.Boundary != boundary {
			return c, errors.New("cursor_expired")
		}
	}
	return c, nil
}
func next(c Cursor, after string) string {
	c.After = after
	return base64.RawURLEncoding.EncodeToString([]byte(encode(c)))
}

const sessionVersionCountSQL = "SELECT count(*) FROM artifact_versions v JOIN artifacts a USING(record_key) WHERE a.conversation_key=s.conversation_key"

func (s *Store) Sessions(ctx context.Context, ns []string, cursor string, limit int) (Page[Session], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.cursor(ns, "sessions", cursor, limit)
	if err != nil {
		return Page[Session]{}, err
	}
	result := Page[Session]{Items: []Session{}, Boundary: c.Boundary}
	in, args := scopeSQL(ns)
	args = append(args, c.After, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT s.conversation_key,s.namespace,src.provider,s.native_id,
 coalesce(title.title,s.native_id),coalesce(title.record_key,''),coalesce(title.source_revision,''),coalesce(title.origin,''),coalesce(title.ordering_ambiguous,0),
 (`+sessionVersionCountSQL+`),
 CASE WHEN EXISTS(SELECT 1 FROM stream_locators l JOIN streams st USING(stream_key) WHERE st.namespace=s.namespace AND l.conversation_id=s.native_id AND l.active=1 AND l.health='available') THEN 'available'
 WHEN EXISTS(SELECT 1 FROM stream_locators l JOIN streams st USING(stream_key) WHERE st.namespace=s.namespace AND l.conversation_id=s.native_id AND l.active=1 AND l.health='blocked') THEN 'blocked'
 WHEN EXISTS(SELECT 1 FROM stream_locators l JOIN streams st USING(stream_key) WHERE st.namespace=s.namespace AND l.conversation_id=s.native_id AND l.active=1 AND l.health='capture_gaps') THEN 'capture_gaps'
 WHEN EXISTS(SELECT 1 FROM stream_locators l JOIN streams st USING(stream_key) WHERE st.namespace=s.namespace AND l.conversation_id=s.native_id AND l.active=1 AND l.health NOT IN ('unavailable','unknown')) THEN 'partial'
 WHEN EXISTS(SELECT 1 FROM stream_locators l JOIN streams st USING(stream_key) WHERE st.namespace=s.namespace AND l.conversation_id=s.native_id AND l.active=1 AND l.health='unknown') THEN 'unknown'
 ELSE 'unavailable' END,
 coalesce(act.signal,''),coalesce(act.signal_epoch,-1),coalesce(act.signal_ordinal,-1),
 coalesce(act.seen_epoch,-1),coalesce(act.seen_ordinal,-1),act.seen_time,coalesce(act.ordering_ambiguous,0)
 FROM sessions s JOIN sources src USING(namespace)
 LEFT JOIN session_titles title USING(conversation_key)
 LEFT JOIN session_activity act ON act.conversation_key=s.conversation_key
 WHERE s.namespace IN (`+in+`) AND s.conversation_key>? ORDER BY s.conversation_key LIMIT ?`, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var item Session
		var titleKey, titleRevision string
		var signal string
		var sigEpoch, sigOrd, seenEpoch, seenOrd int64
		var seenTime sql.NullString
		var actAmbiguous int
		if err := rows.Scan(&item.Key, &item.Namespace, &item.Provider, &item.NativeID, &item.Title, &titleKey, &titleRevision, &item.TitleKind, &item.TitleOrderingAmbiguous, &item.RecordVersions, &item.SourceHealth, &signal, &sigEpoch, &sigOrd, &seenEpoch, &seenOrd, &seenTime, &actAmbiguous); err != nil {
			return result, err
		}
		item.IsSubagent = item.Provider == sessioncapture.Claude && sessioncapture.IsClaudeSubagentConversation(item.NativeID)
		if titleKey != "" {
			item.TitleRef = &sessionrecord.Ref{RecordKey: titleKey, SourceRevision: titleRevision}
		}
		// A consumed input request (newer evidence exists) reads as working.
		item.Activity = signal
		if signal == "input" && (seenEpoch > sigEpoch || (seenEpoch == sigEpoch && seenOrd > sigOrd)) {
			item.Activity = "working"
		}
		if item.Activity == "" || item.Activity == "none" {
			item.Activity = "unknown"
		}
		item.ActivityAmbiguous = actAmbiguous != 0
		if seenTime.Valid {
			item.LastRecordTime = &seenTime.String
		}
		result.Items = append(result.Items, item)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if len(result.Items) > limit {
		result.Items = result.Items[:limit]
		result.Next = next(c, result.Items[limit-1].Key)
	}
	rows.Close()
	for i := range result.Items {
		item := &result.Items[i]
		projects, err := s.db.QueryContext(ctx, "SELECT project,record_key,source_revision FROM session_projects WHERE conversation_key=? ORDER BY project LIMIT 33", item.Key)
		if err != nil {
			return result, err
		}
		item.Projects = []Project{}
		for projects.Next() {
			var p Project
			if err := projects.Scan(&p.Path, &p.Ref.RecordKey, &p.Ref.SourceRevision); err != nil {
				projects.Close()
				return result, err
			}
			item.Projects = append(item.Projects, p)
		}
		err = projects.Err()
		projects.Close()
		if err != nil {
			return result, err
		}
		if len(item.Projects) > 32 {
			item.Projects = item.Projects[:32]
			item.ProjectsTruncated = true
		}
	}

	return result, nil
}

func (s *Store) Artifacts(ctx context.Context, ns []string, conversation, cursor string, limit int) (Page[Artifact], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.artifacts(ctx, ns, conversation, cursor, limit, false, true)
}
func (s *Store) artifacts(ctx context.Context, ns []string, conversation, cursor string, limit int, reverse, history bool) (Page[Artifact], error) {
	kind := "artifacts:" + conversation
	comparator, direction, filter := ">", "ASC", ""
	if reverse {
		kind = "transcript:" + conversation
		comparator = "<"
		direction = "DESC"
	}
	if !history {
		kind += ":current"
		filter = " AND current=1"
	}
	c, err := s.cursor(ns, kind, cursor, limit)
	if err != nil {
		return Page[Artifact]{}, err
	}
	result := Page[Artifact]{Items: []Artifact{}, Boundary: c.Boundary}
	in, args := scopeSQL(ns)
	var count int
	sessionArgs := append(append([]any{}, args...), conversation)
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE namespace IN ("+in+") AND conversation_key=?", sessionArgs...).Scan(&count); err != nil {
		return result, err
	}
	if count == 0 {
		return result, errors.New("not_found")
	}
	after := c.After
	if reverse && after == "" {
		after = "~"
	}
	args = append(args, conversation, after, limit+1)
	rows, err := s.db.QueryContext(ctx, `WITH positions AS (
 SELECT n.record_key,n.source_revision,n.adapter_version,n.kind,n.source_time,
 coalesce(h.source_revision=n.source_revision AND h.adapter_version=n.adapter_version,0) AS current,
 o.stream_key,o.generation,g.epoch,o.ordinal,
 o.stream_key||'/'||printf('%020d',g.epoch)||'/'||printf('%020d',o.ordinal)||'/'||n.record_key||'/'||n.source_revision||'/'||n.adapter_version AS ordering,
 row_number() OVER (PARTITION BY n.record_key,n.source_revision,n.adapter_version ORDER BY o.stream_key,g.epoch DESC,o.ordinal) AS chosen
 FROM normalizations n JOIN artifacts a USING(record_key) LEFT JOIN record_heads h USING(record_key)
 JOIN record_observations o ON o.record_key=n.record_key AND o.source_revision=n.source_revision
 JOIN stream_generations g ON g.stream_key=o.stream_key AND g.generation=o.generation
 WHERE a.namespace IN (`+in+`) AND a.conversation_key=?)
 SELECT record_key,source_revision,adapter_version,kind,source_time,current,stream_key,generation,epoch,ordinal,ordering
 FROM positions WHERE chosen=1`+filter+` AND ordering`+comparator+`? ORDER BY ordering `+direction+` LIMIT ?`, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	var orders []string
	for rows.Next() {
		var item Artifact
		var order string
		if err := rows.Scan(&item.Key, &item.Revision, &item.Adapter, &item.Kind, &item.SourceTime, &item.Current, &item.Stream, &item.Generation, &item.Epoch, &item.Ordinal, &order); err != nil {
			return result, err
		}
		result.Items = append(result.Items, item)
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if len(result.Items) > limit {
		result.Items = result.Items[:limit]
		result.Next = next(c, orders[limit-1])
	}
	return result, nil
}

func (s *Store) Get(ctx context.Context, ns []string, key, revision, adapter string, raw bool) (Exact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(ctx, ns, key, revision, adapter, raw)
}
func (s *Store) get(ctx context.Context, ns []string, key, revision, adapter string, raw bool) (Exact, error) {
	var out Exact
	if key == "" || !sessionrecord.ValidDigest(revision) || adapter == "" {
		return out, errors.New("exact_record_revision_and_adapter_required")
	}
	in, args := scopeSQL(ns)
	var namespace, conversation string
	err := s.db.QueryRowContext(ctx, "SELECT namespace,conversation_key FROM artifacts WHERE namespace IN ("+in+") AND record_key=?", append(append([]any{}, args...), key)...).Scan(&namespace, &conversation)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	if err == nil {
		if err := checkSuppression(ctx, s.db, namespace, conversation, key); err != nil {
			if err.Error() == "capture_suppressed" {
				return out, errors.New("evidence_redacted")
			}
			return out, err
		}
	}
	args = append(args, key, revision, adapter)
	var envelope string
	var original []byte
	join := ` FROM normalizations n JOIN artifacts a USING(record_key)
 JOIN content_objects c ON c.digest=n.source_revision WHERE a.namespace IN (` + in + `)
 AND n.record_key=? AND n.source_revision=? AND n.adapter_version=?`
	var envelopeBytes, originalBytes int64
	err = s.db.QueryRowContext(ctx, "SELECT length(CAST(n.envelope_json AS BLOB)),c.byte_length"+join, args...).Scan(&envelopeBytes, &originalBytes)
	if err == sql.ErrNoRows {
		return out, errors.New("not_found")
	}
	if err != nil {
		return out, err
	}
	if envelopeBytes > MaxResponseBytes || originalBytes > 64<<20 {
		return out, errors.New("response_too_large")
	}
	if err := s.db.QueryRowContext(ctx, "SELECT n.envelope_json,c.original_bytes"+join, args...).Scan(&envelope, &original); err != nil {
		return out, err
	}
	rec, err := sessionrecord.Decode([]byte(envelope))
	if err != nil {
		return out, err
	}
	if err := rec.VerifyRaw(original); err != nil {
		return out, err
	}
	sanitized, err := sessioncapture.Sanitize(sessioncapture.Captured{Record: rec, Raw: original})
	if err != nil {
		return Exact{}, err
	}
	rec, original = sanitized.Record, sanitized.Raw
	out.Record = rec
	if raw {
		out.Raw = original
	}
	out.Boundary, err = s.boundary()
	if err == nil && len(encode(out)) > MaxResponseBytes {
		return Exact{}, errors.New("response_too_large")
	}
	return out, err
}

func (s *Store) Health(ctx context.Context, ns []string) ([]SourceHealth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health(ctx, ns)
}
func (s *Store) health(ctx context.Context, ns []string) ([]SourceHealth, error) {
	in, args := scopeSQL(ns)
	rows, err := s.db.QueryContext(ctx, `SELECT st.stream_key,st.namespace,src.provider,l.active,l.health,l.checked_at,
 cp.stream_key IS NOT NULL,cp.generation,json_extract(cp.checkpoint_json,'$.epoch'),cp.ordinal,
 coalesce(cp.captured_offset,0),coalesce(cp.parsed_offset,0),coalesce(cp.has_gaps,0),
 (SELECT o.observed_at FROM record_observations o
  WHERE o.stream_key=cp.stream_key AND o.generation=cp.generation AND o.ordinal=cp.ordinal-1 LIMIT 1),
 (SELECT n.source_time FROM record_observations o JOIN normalization_storage n
  ON n.record_key=o.record_key AND n.source_revision=o.source_revision
  WHERE o.stream_key=cp.stream_key AND o.generation=cp.generation AND o.ordinal=cp.ordinal-1 ORDER BY n.rowid DESC LIMIT 1)
 FROM stream_locators l JOIN streams st USING(stream_key) JOIN sources src USING(namespace)
 LEFT JOIN ingest_checkpoints cp USING(stream_key)
 WHERE st.namespace IN (`+in+`) AND l.active=1 ORDER BY st.stream_key LIMIT ?`, append(args, MaxEnrolledSources)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SourceHealth{}
	for rows.Next() {
		var h SourceHealth
		if err := rows.Scan(&h.Key, &h.Namespace, &h.Provider, &h.Enrolled, &h.Health, &h.CheckedAt, &h.CheckpointAvailable, &h.Generation, &h.Epoch, &h.NextOrdinal, &h.CapturedOffset, &h.ParsedOffset, &h.HasGaps, &h.LastCapturedAt, &h.LastSourceTime); err != nil {
			return nil, err
		}
		if check, ok := s.healthChecks[h.Key]; ok && check.health == h.Health {
			checked := stamp(check.checkedAt)
			h.CheckedAt = &checked
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) Status(ctx context.Context, namespaces []string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status, err := s.stats(ctx, namespaces)
	if err != nil {
		return nil, err
	}
	sources, err := s.health(ctx, namespaces)
	if err != nil {
		return nil, err
	}
	in, args := scopeSQL(namespaces)
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM stream_locators l JOIN streams st USING(stream_key)
 WHERE st.namespace IN (`+in+`) AND l.active=1`, args...).Scan(&total); err != nil {
		return nil, err
	}
	quarantine, err := s.quarantineStatus(ctx, namespaces, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	status["quarantine"] = quarantine
	status["sources"] = sources
	status["sources_total"] = total
	status["sources_omitted"] = total - int64(len(sources))
	return status, nil
}

// Scope returns a requested narrowing, never a widening, of the local read corpus.
func Scope(allowed []string, requested string) ([]string, error) {
	if requested == "" {
		return append([]string{}, allowed...), nil
	}
	for _, n := range allowed {
		if n == requested {
			return []string{n}, nil
		}
	}
	return nil, errors.New("scope_denied")
}

func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	// Storage/IO errors must not leak database paths or transcript text over API.
	switch err.Error() {
	case "backup_object_limit", "secret_redaction_required", "not_found", "invalid_request", "invalid_cursor", "cursor_expired", "invalid_page_limit", "response_too_large", "scope_denied", "exact_record_revision_and_adapter_required", "capture_capacity_limit",
		"invalid_archive_feed_cursor", "archive_feed_cursor_expired", "unsupported_archive_feed_version", "evidence_redacted", "capture_spool_pending",
		"redaction_target_required", "invalid_redaction_scope", "redaction_scope_mismatch", "redaction_target_unavailable":
		return err.Error()
	}
	if strings.HasPrefix(err.Error(), "source_") {
		return "source_unavailable"
	}
	return "archive_error"
}
