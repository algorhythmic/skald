package archive

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"

	"github.com/algorhythmic/skald/archivefeed"
	"github.com/algorhythmic/skald/sessionrecord"
)

const maxFeedCursorBytes = 4096

var ErrFeedCursor = errors.New("invalid_archive_feed_cursor")
var ErrFeedCursorExpired = errors.New("archive_feed_cursor_expired")

type feedCursor struct {
	Version   int    `json:"version"`
	Instance  string `json:"instance"`
	Scope     string `json:"scope"`
	HighWater int64  `json:"high_water"`
	After     int64  `json:"after"`
	Phase     string `json:"phase"`
}

// Changes returns metadata events for the exact authorized namespace set. The
// initial finite snapshot is the committed event history through HighWater;
// subsequent pages tail newer changes. History is intentional: it preserves
// corrections and gaps that a latest-head projection would silently erase.
// Callers authorize namespaces before invoking this method.
func (s *Store) Changes(ctx context.Context, namespaces []string, scopeGeneration int64, cursor string, limit int) (archivefeed.Page, error) {
	if limit < 1 || limit > archivefeed.MaxPageEvents {
		return archivefeed.Page{}, errors.New("invalid_page_limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ns := append([]string(nil), namespaces...)
	sort.Strings(ns)
	for i, n := range ns {
		if n == "" || (i > 0 && ns[i-1] == n) {
			return archivefeed.Page{}, errors.New("invalid_read_scope")
		}
	}
	if scopeGeneration < 0 {
		return archivefeed.Page{}, errors.New("invalid_read_scope_generation")
	}
	scope := sessionrecord.Key("archive-feed-scope", append([]string{strconv.FormatInt(scopeGeneration, 10)}, ns...)...)
	c := feedCursor{Version: archivefeed.Version, Instance: s.instance, Scope: scope, Phase: "snapshot"}
	if cursor == "" {
		var err error
		c.HighWater, err = s.boundary()
		if err != nil {
			return archivefeed.Page{}, err
		}
	} else {
		if len(cursor) > maxFeedCursorBytes {
			return archivefeed.Page{}, ErrFeedCursor
		}
		wire, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || len(wire) > maxFeedCursorBytes {
			return archivefeed.Page{}, ErrFeedCursor
		}
		decoder := json.NewDecoder(bytes.NewReader(wire))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&c) != nil {
			return archivefeed.Page{}, ErrFeedCursor
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF || c.HighWater < 0 || c.After < 0 || (c.Phase != "snapshot" && c.Phase != "tail") || (c.Phase == "snapshot" && c.After > c.HighWater) || (c.Phase == "tail" && c.After < c.HighWater) {
			return archivefeed.Page{}, ErrFeedCursor
		}
		if c.Version != archivefeed.Version {
			return archivefeed.Page{}, archivefeed.ErrUnsupportedVersion
		}
		if c.Instance != s.instance || c.Scope != scope {
			return archivefeed.Page{}, ErrFeedCursorExpired
		}
		var current int64
		if current, err = s.boundary(); err != nil {
			return archivefeed.Page{}, err
		}
		if c.HighWater > current || c.After > current {
			return archivefeed.Page{}, ErrFeedCursor
		}
	}
	upper := c.HighWater
	if c.Phase == "tail" {
		var err error
		upper, err = s.boundary()
		if err != nil {
			return archivefeed.Page{}, err
		}
	}
	page := archivefeed.Page{Version: archivefeed.Version, Phase: c.Phase, HighWater: c.HighWater, Events: []archivefeed.Event{}}
	in, args := scopeSQL(ns)
	args = append(args, c.After, upper, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,change_kind,namespace,coalesce(record_key,''),coalesce(source_revision,''),payload_json
 FROM changes WHERE namespace IN (`+in+`) AND sequence>? AND sequence<=? ORDER BY sequence LIMIT ?`, args...)
	if err != nil {
		return archivefeed.Page{}, err
	}
	type rawChange struct {
		sequence                                int64
		kind, namespace, key, revision, payload string
	}
	var changes []rawChange
	for rows.Next() {
		var ch rawChange
		if err := rows.Scan(&ch.sequence, &ch.kind, &ch.namespace, &ch.key, &ch.revision, &ch.payload); err != nil {
			rows.Close()
			return archivefeed.Page{}, err
		}
		changes = append(changes, ch)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return archivefeed.Page{}, err
	}
	rows.Close()
	more := len(changes) > limit
	page.More = more
	if more {
		changes = changes[:limit]
	}
	for _, ch := range changes {
		event, err := s.feedEvent(ctx, ch.sequence, ch.kind, ch.namespace, ch.key, ch.revision, ch.payload)
		if err != nil {
			return archivefeed.Page{}, err
		}
		page.Events = append(page.Events, event)
	}
	if len(page.Events) > 0 {
		c.After = page.Events[len(page.Events)-1].Sequence
	}
	if c.Phase == "snapshot" && !more {
		c.Phase = "tail"
		c.After = c.HighWater
	}
	page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(encode(c)))
	return page, nil
}

type feedPayload struct {
	Stream     string `json:"stream"`
	Generation string `json:"generation"`
	Ordinal    int64  `json:"ordinal"`
	Adapter    string `json:"adapter_version"`
	Correction bool   `json:"correction"`
	Code       string `json:"code"`
	Offset     int64  `json:"offset"`
	Health     string `json:"health"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
	ScopeKind  string `json:"scope_kind"`
	ScopeKey   string `json:"scope_key"`
}

func (s *Store) feedEvent(ctx context.Context, seq int64, kind, namespace, key, revision, payload string) (archivefeed.Event, error) {
	var p feedPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return archivefeed.Event{}, err
	}
	e := archivefeed.Event{Sequence: seq, Namespace: namespace, StreamKey: p.Stream}
	switch kind {
	case "record":
		e.Kind = "record"
		e.RecordKey, e.SourceRevision = key, revision
		if p.Correction {
			e.Kind = "correction"
		}
		adapter := p.Adapter
		if adapter == "" {
			err := s.db.QueryRowContext(ctx, `SELECT adapter_version FROM normalizations WHERE record_key=? AND source_revision=? ORDER BY adapter_version LIMIT 1`, key, revision).Scan(&adapter)
			if err != nil && err != sql.ErrNoRows {
				return e, err
			}
		}
		var available int
		err := s.db.QueryRowContext(ctx, `SELECT 1 FROM normalizations WHERE record_key=? AND source_revision=? AND adapter_version=?`, key, revision, adapter).Scan(&available)
		if err != nil && err != sql.ErrNoRows {
			return e, err
		}
		missing := adapter == "" || err == sql.ErrNoRows
		var redacted int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM purge_tombstones p JOIN artifacts a ON a.namespace=p.namespace
 WHERE a.record_key=? AND a.namespace=? AND
 ((p.scope_kind='source' AND p.scope_key=a.namespace) OR
  (p.scope_kind='conversation' AND p.scope_key=a.conversation_key) OR
  (p.scope_kind='record' AND p.scope_key=a.record_key))`, key, namespace).Scan(&redacted); err != nil {
			return e, err
		}
		if redacted > 0 {
			e.Unavailable = "redacted"
		} else if missing {
			e.Unavailable = "evidence_missing"
		} else {
			e.Locator = &archivefeed.Locator{RecordKey: key, SourceRevision: revision, AdapterVersion: adapter}
		}
		e.Generation = p.Generation
		e.Ordinal = &p.Ordinal
	case "gap":
		e.Kind = "gap"
		e.Generation, e.GapCode, e.GapOffset, e.Ordinal = p.Generation, p.Code, &p.Offset, &p.Ordinal
		if e.StreamKey == "" {
			// Pre-feed rows omitted the stream key. Recover it only when the
			// persisted gap identity resolves unambiguously in this namespace.
			rows, err := s.db.QueryContext(ctx, `SELECT g.stream_key FROM capture_gaps g JOIN streams st USING(stream_key)
 WHERE st.namespace=? AND g.generation=? AND g.source_offset=? AND g.code=? LIMIT 2`, namespace, p.Generation, p.Offset, p.Code)
			if err != nil {
				return e, err
			}
			var keys []string
			for rows.Next() {
				var key string
				if err := rows.Scan(&key); err != nil {
					rows.Close()
					return e, err
				}
				keys = append(keys, key)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return e, err
			}
			rows.Close()
			if len(keys) == 1 {
				e.StreamKey = keys[0]
			}
		}
	case "source":
		if p.State == "removed" || p.State == "unconfigured" {
			e.Kind, e.Unavailable = "source_removed", "native_source_removed"
		} else {
			e.Kind, e.Health = "source_health", p.Health
			if e.Health == "" {
				e.Health = p.State
			}
		}
	case "withdrawal":
		e.Kind, e.Unavailable = "redaction", "redacted"
		e.ScopeKind, e.ScopeKey = p.ScopeKind, p.ScopeKey
		if e.ScopeKind == "" && key != "" {
			e.ScopeKind, e.ScopeKey = "record", key
		}
	default:
		return e, errors.New("unknown_archive_change_kind")
	}
	return e, nil
}
