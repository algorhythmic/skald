package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"skald/sessioncapture"
	"skald/sessionrecord"
)

type Registration struct {
	sessioncapture.Source
	Root        string   `json:"root"`
	Path        string   `json:"path"`
	RootAliases []string `json:"root_aliases,omitempty"`
}

func (r Registration) Validate() error {
	if err := r.Source.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(r.Root) || !filepath.IsLocal(r.Path) || len(r.RootAliases) > 16 || len(r.Namespace) > 256 || len(r.StreamID) > 256 {
		return errors.New("invalid_source_registration")
	}
	for _, alias := range r.RootAliases {
		if !filepath.IsAbs(alias) {
			return errors.New("absolute_root_alias_required")
		}
	}
	return nil
}

// Register pins namespace/provider and explicit root aliases. A changed root must
// name a previously registered root as an alias; paths never redefine source keys.
func (s *Store) Register(ctx context.Context, registrations []Registration) error {
	return s.register(ctx, registrations, true)
}

// Enroll adds verified streams or updates an explicitly verified locator without
// deactivating the other configured sources.
func (s *Store) Enroll(ctx context.Context, registrations []Registration) error {
	return s.register(ctx, registrations, false)
}
func (s *Store) register(ctx context.Context, registrations []Registration, replace bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if replace {
		if _, err := tx.ExecContext(ctx, "UPDATE stream_locators SET active=0,health='unconfigured'"); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, r := range registrations {
		if err := r.Validate(); err != nil {
			return err
		}
		if seen[r.Key()] {
			return errors.New("duplicate_stream_registration")
		}
		seen[r.Key()] = true
		var provider string
		err := tx.QueryRowContext(ctx, "SELECT provider FROM sources WHERE namespace=?", r.Namespace).Scan(&provider)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && provider != r.Provider {
			return errors.New("namespace_provider_conflict")
		}
		if err == sql.ErrNoRows {
			if _, err := tx.ExecContext(ctx, "INSERT INTO sources VALUES (?,?,?,?,?)", r.Namespace, r.Provider, sessioncapture.AdapterVersion, encode(sessioncapture.Probe(r.Provider, r.ProviderVersion)), `{"kind":"configured_local"}`); err != nil {
				return err
			}
		} else {
			rows, err := tx.QueryContext(ctx, "SELECT canonical_root FROM source_roots WHERE namespace=?", r.Namespace)
			if err != nil {
				return err
			}
			known := false
			for rows.Next() {
				var root string
				if err := rows.Scan(&root); err != nil {
					rows.Close()
					return err
				}
				if root == r.Root || slices.Contains(r.RootAliases, root) {
					known = true
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if !known {
				return errors.New("root_relocation_requires_explicit_alias")
			}
		}
		for _, root := range append([]string{r.Root}, r.RootAliases...) {
			if _, err := tx.ExecContext(ctx, "INSERT INTO source_roots VALUES (?,?,'configured_by_user') ON CONFLICT DO NOTHING", r.Namespace, root); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO streams VALUES (?,?,?) ON CONFLICT DO NOTHING", r.Key(), r.Namespace, r.StreamID); err != nil {
			return err
		}
		var oldID string
		err = tx.QueryRowContext(ctx, "SELECT conversation_id FROM stream_locators WHERE stream_key=?", r.Key()).Scan(&oldID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if oldID != "" && r.ConversationID != "" && oldID != r.ConversationID {
			return errors.New("stream_conversation_conflict")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO stream_locators VALUES (?,?,?,1,'unknown',NULL,?)
   ON CONFLICT(stream_key) DO UPDATE SET root=excluded.root,relative_path=excluded.relative_path,active=1,health='unknown'`, r.Key(), r.Root, r.Path, r.ConversationID); err != nil {
			return err
		}
	}
	// Any configuration reload invalidates read cursors, including scope narrowing.
	for _, r := range registrations {
		if _, err := tx.ExecContext(ctx, "INSERT INTO changes(change_kind,namespace,payload_json) VALUES ('source',?,?)", r.Namespace, `{"state":"configured"}`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Checkpoint(ctx context.Context, key string) (sessioncapture.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return checkpoint(ctx, s.db, key)
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func checkpoint(ctx context.Context, q rowQuerier, key string) (sessioncapture.Checkpoint, error) {
	var cp sessioncapture.Checkpoint
	var data string
	err := q.QueryRowContext(ctx, "SELECT checkpoint_json FROM ingest_checkpoints WHERE stream_key=?", key).Scan(&data)
	if err == sql.ErrNoRows {
		return cp, nil
	}
	if err != nil {
		return cp, err
	}
	if err := json.Unmarshal([]byte(data), &cp); err != nil {
		return cp, errors.New("corrupt_checkpoint")
	}
	return cp, nil
}

func (s *Store) Ingest(ctx context.Context, r Registration, previous sessioncapture.Checkpoint, batch sessioncapture.Batch, now time.Time) (finalErr error) {
	defer func() { finalErr = capacityError(finalErr) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := r.Validate(); err != nil {
		return err
	}
	cp := batch.Checkpoint
	if cp.Version != 1 || cp.SourceKey != r.Key() || !sessionrecord.ValidDigest(cp.PrefixDigest) || cp.Offset < 0 || cp.Ordinal < 0 || cp.ParsedOffset < 0 || cp.ParsedOffset > cp.Offset {
		return errors.New("invalid_candidate_checkpoint")
	}
	if cp.Epoch < previous.Epoch || cp.Epoch > previous.Epoch+1 || (cp.Epoch == previous.Epoch && (cp.Offset < previous.Offset || cp.Ordinal < previous.Ordinal)) {
		return errors.New("checkpoint_regression")
	}
	var needed uint64
	baseOffset, baseOrdinal := previous.Offset, previous.Ordinal
	if cp.Epoch != previous.Epoch {
		baseOffset = 0
		baseOrdinal = 0
	}
	if previous.Generation != "" && cp.Epoch == previous.Epoch && cp.Generation != previous.Generation {
		return errors.New("generation_conflict")
	}
	for i, item := range batch.Records {
		rec := item.Record
		if err := rec.VerifyRaw(item.Raw); err != nil {
			return err
		}
		if rec.Provider != r.Provider || rec.SourceNamespace != r.Namespace || rec.ConversationKey == nil || *rec.ConversationKey != sessionrecord.Conversation(r.Namespace, cp.ConversationID) || rec.SourceOrder == nil || rec.SourceOrder.StreamGeneration != cp.Generation {
			return errors.New("batch_source_mismatch")
		}
		if rec.SourceOrder.Ordinal != baseOrdinal+int64(i) {
			return errors.New("candidate_order_mismatch")
		}
		baseOffset += int64(len(item.Raw))
		needed += uint64(len(item.Raw) + len(encode(rec))*2 + 4096)
	}
	if cp.Offset != baseOffset || cp.Ordinal != baseOrdinal+int64(len(batch.Records)) {
		return errors.New("candidate_boundary_mismatch")
	}
	if err := s.checkSpace(needed); err != nil {
		return err
	}
	if needed > 0 {
		var pages, size int64
		if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
			return err
		}
		if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
			return err
		}
		if pages*size >= s.options.MaxDatabaseBytes {
			return ErrCapacity
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stored, err := checkpoint(ctx, tx, r.Key())
	if err != nil {
		return err
	}
	if stored != previous {
		return ErrCheckpoint
	}
	var active int
	if err := tx.QueryRowContext(ctx, "SELECT active FROM stream_locators WHERE stream_key=?", r.Key()).Scan(&active); err != nil {
		return err
	}
	if active != 1 {
		return errors.New("source_not_registered")
	}
	if cp.Generation != "" {
		if _, err := tx.ExecContext(ctx, "INSERT INTO stream_generations VALUES (?,?,?) ON CONFLICT DO NOTHING", r.Key(), cp.Generation, cp.Epoch); err != nil {
			return err
		}
	}
	if cp.ConversationID != "" {
		if _, err := tx.ExecContext(ctx, "INSERT INTO sessions VALUES (?,?,?,'unknown') ON CONFLICT DO NOTHING", sessionrecord.Conversation(r.Namespace, cp.ConversationID), r.Namespace, cp.ConversationID); err != nil {
			return err
		}
	}
	for _, item := range batch.Records {
		rec := item.Record
		var suppressed int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM purge_tombstones WHERE namespace=? AND
   ((scope_kind='source' AND scope_key=?) OR (scope_kind='conversation' AND scope_key=?) OR (scope_kind='record' AND scope_key=?))`, r.Namespace, r.Namespace, *rec.ConversationKey, rec.RecordKey).Scan(&suppressed); err != nil {
			return err
		}
		if suppressed > 0 {
			return errors.New("capture_suppressed")
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO content_objects VALUES (?,?,'application/jsonl',?) ON CONFLICT DO NOTHING", rec.SourceRevision, len(item.Raw), item.Raw); err != nil {
			return capacityError(err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO artifacts VALUES (?,?,?,?,?) ON CONFLICT DO NOTHING", rec.RecordKey, r.Namespace, rec.OriginScope.Kind, rec.OriginScope.Key, *rec.ConversationKey); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO artifact_versions VALUES (?,?,?) ON CONFLICT DO NOTHING", rec.RecordKey, rec.SourceRevision, stamp(rec.ObservedAt)); err != nil {
			return err
		}
		var sourceTime any
		if rec.SourceTime != nil {
			sourceTime = stamp(*rec.SourceTime)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO normalizations VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING", rec.RecordKey, rec.SourceRevision, rec.AdapterVersion, rec.ContractVersion, rec.Kind, sourceTime, rec.SourceOrder.StreamGeneration, rec.SourceOrder.Ordinal, encode(rec)); err != nil {
			return capacityError(err)
		}
		result, err := tx.ExecContext(ctx, "INSERT INTO record_observations VALUES (?,?,?,?,?,?) ON CONFLICT DO NOTHING", r.Key(), cp.Generation, rec.SourceOrder.Ordinal, rec.RecordKey, rec.SourceRevision, stamp(now))
		if err != nil {
			return err
		}
		inserted, _ := result.RowsAffected()
		if inserted > 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO record_heads VALUES (?,?,?) ON CONFLICT(record_key) DO UPDATE SET source_revision=excluded.source_revision,adapter_version=excluded.adapter_version`, rec.RecordKey, rec.SourceRevision, rec.AdapterVersion); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO changes(change_kind,namespace,record_key,source_revision,payload_json) VALUES ('record',?,?,?,?)", r.Namespace, rec.RecordKey, rec.SourceRevision, encode(map[string]any{"stream": r.Key(), "generation": cp.Generation, "ordinal": rec.SourceOrder.Ordinal})); err != nil {
				return err
			}
			if rec.Kind == "title" && rec.Body.Text != "" {
				text := []rune(rec.Body.Text)
				if len(text) > 240 {
					text = text[:240]
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO session_titles VALUES (?,?,?,?,?,?,?,?,0)
 ON CONFLICT(conversation_key) DO UPDATE SET
 record_key=excluded.record_key,source_revision=excluded.source_revision,adapter_version=excluded.adapter_version,
 title=excluded.title,epoch=excluded.epoch,ordinal=excluded.ordinal
 WHERE session_titles.stream_key=excluded.stream_key AND (excluded.epoch>session_titles.epoch OR
 (excluded.epoch=session_titles.epoch AND excluded.ordinal>=session_titles.ordinal))`, *rec.ConversationKey, rec.RecordKey, rec.SourceRevision, rec.AdapterVersion, string(text), r.Key(), cp.Epoch, rec.SourceOrder.Ordinal); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, "UPDATE session_titles SET ordering_ambiguous=1 WHERE conversation_key=? AND stream_key!=?", *rec.ConversationKey, r.Key()); err != nil {
					return err
				}
			}
		}
		var cwd string
		if raw, ok := rec.Extensions["native_cwd"]; ok && json.Unmarshal(raw, &cwd) == nil && cwd != "" && len(cwd) <= 4096 {
			if _, err := tx.ExecContext(ctx, "INSERT INTO session_projects VALUES (?,?,?,?) ON CONFLICT DO NOTHING", *rec.ConversationKey, cwd, rec.RecordKey, rec.SourceRevision); err != nil {
				return err
			}
		}
	}
	for _, gap := range batch.Gaps {
		result, err := tx.ExecContext(ctx, "INSERT INTO capture_gaps VALUES (?,?,?,?,?) ON CONFLICT DO NOTHING", r.Key(), gap.Generation, gap.Offset, gap.Code, stamp(now))
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n > 0 {
			if _, err := tx.ExecContext(ctx, "INSERT INTO changes(change_kind,namespace,payload_json) VALUES ('gap',?,?)", r.Namespace, encode(gap)); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ingest_checkpoints VALUES (?,?,?,?,?,?,?) ON CONFLICT(stream_key) DO UPDATE SET generation=excluded.generation,captured_offset=excluded.captured_offset,parsed_offset=excluded.parsed_offset,ordinal=excluded.ordinal,checkpoint_json=excluded.checkpoint_json,has_gaps=excluded.has_gaps`, r.Key(), cp.Generation, cp.Offset, cp.ParsedOffset, cp.Ordinal, encode(cp), cp.HasGaps); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE stream_locators SET conversation_id=? WHERE stream_key=?", cp.ConversationID, r.Key()); err != nil {
		return err
	}
	health := "available"
	if batch.Pending {
		health = "partial_line"
	}
	if batch.More {
		health = "backfilling"
	}
	if cp.HasGaps {
		health = "capture_gaps"
	}
	if batch.Blocked {
		health = "blocked"
	}
	if err := setHealth(ctx, tx, r.Key(), r.Namespace, health, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sources SET capabilities_json=?,adapter_version=? WHERE namespace=?", encode(sessioncapture.Probe(r.Provider, cp.ProviderVersion)), sessioncapture.AdapterVersion, r.Namespace); err != nil {
		return err
	}
	return capacityError(tx.Commit())
}

func capacityError(err error) error {
	if err != nil && (strings.Contains(err.Error(), "database or disk is full") || strings.Contains(err.Error(), "SQLITE_FULL")) {
		return ErrCapacity
	}
	return err
}
func setHealth(ctx context.Context, tx *sql.Tx, key, namespace, health string, now time.Time) error {
	var previous string
	if err := tx.QueryRowContext(ctx, "SELECT health FROM stream_locators WHERE stream_key=?", key).Scan(&previous); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE stream_locators SET health=?,checked_at=? WHERE stream_key=?", health, stamp(now), key); err != nil {
		return err
	}
	if previous != health {
		_, err := tx.ExecContext(ctx, "INSERT INTO changes(change_kind,namespace,payload_json) VALUES ('source',?,?)", namespace, encode(map[string]string{"stream": key, "health": health}))
		return err
	}
	return nil
}
func (s *Store) SetHealth(ctx context.Context, r Registration, health string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setHealth(ctx, tx, r.Key(), r.Namespace, health, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Stats(ctx context.Context, namespaces []string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats(ctx, namespaces)
}
func (s *Store) stats(ctx context.Context, namespaces []string) (map[string]any, error) {
	in, args := scopeSQL(namespaces)
	var sessions, versions int64
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE namespace IN ("+in+")", args...).Scan(&sessions); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM artifact_versions v JOIN artifacts a USING(record_key) WHERE a.namespace IN ("+in+")", args...).Scan(&versions); err != nil {
		return nil, err
	}
	boundary, err := s.boundary()
	if err != nil {
		return nil, err
	}
	return map[string]any{"archive_instance": s.instance, "schema_version": SchemaVersion, "contract_version": 1, "boundary": boundary, "sessions": sessions, "record_versions": versions, "retrieval": "unavailable", "surface_activation": "unsupported"}, nil
}

func (s *Store) Status(ctx context.Context, namespaces []string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status, err := s.stats(ctx, namespaces)
	if err != nil {
		return nil, err
	}
	status["sources"], err = s.health(ctx, namespaces)
	return status, err
}
func scopeSQL(namespaces []string) (string, []any) {
	if len(namespaces) == 0 {
		return "NULL", nil
	}
	args := make([]any, len(namespaces))
	for i, n := range namespaces {
		args[i] = n
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(args)), ","), args
}
func fail(format string, args ...any) error { return fmt.Errorf(format, args...) }
