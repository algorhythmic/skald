package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/algorhythmic/skald/internal/secretredact"
	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
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
	if len(registrations) > MaxEnrolledSources {
		return errors.New("source_registry_limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	removed := map[string]string{}
	if replace {
		rows, err := tx.QueryContext(ctx, "SELECT stream_key,namespace FROM stream_locators JOIN streams USING(stream_key) WHERE active=1")
		if err != nil {
			return err
		}
		for rows.Next() {
			var key, namespace string
			if err := rows.Scan(&key, &namespace); err != nil {
				rows.Close()
				return err
			}
			removed[key] = namespace
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, r := range registrations {
		r.Source = sessioncapture.PublicSource(r.Source)
		if err := r.Validate(); err != nil {
			return err
		}
		if seen[r.Key()] {
			return errors.New("duplicate_stream_registration")
		}
		seen[r.Key()] = true
		delete(removed, r.Key())
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
   ON CONFLICT(stream_key) DO UPDATE SET root=excluded.root,relative_path=excluded.relative_path,active=1,health='unknown' WHERE (stream_locators.root,stream_locators.relative_path,stream_locators.active,stream_locators.health) IS NOT (excluded.root,excluded.relative_path,1,'unknown')`, r.Key(), r.Root, r.Path, r.ConversationID); err != nil {
			return err
		}
	}
	// Leave retained registrations active rather than deactivating and restoring
	// every locator. Only removed streams need the availability transition.
	for key := range removed {
		if _, err := tx.ExecContext(ctx, "UPDATE stream_locators SET active=0,health='unconfigured' WHERE stream_key=?", key); err != nil {
			return err
		}
	}
	// Bound the selected capture corpus, retaining inactive archived streams.
	var active int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM stream_locators WHERE active=1").Scan(&active); err != nil {
		return err
	}
	if active > MaxEnrolledSources {
		return errors.New("source_registry_limit")
	}
	// Source removal is an availability event; archived evidence remains readable.
	for key, namespace := range removed {
		if _, err := tx.ExecContext(ctx, "INSERT INTO changes(change_kind,namespace,payload_json) VALUES ('source',?,?)", namespace, encode(map[string]string{"stream": key, "state": "unconfigured"})); err != nil {
			return err
		}
	}
	for _, r := range registrations {
		if _, err := tx.ExecContext(ctx, "INSERT INTO changes(change_kind,namespace,payload_json) VALUES ('source',?,?)", r.Namespace, encode(map[string]string{"stream": r.Key(), "state": "configured"})); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Only locators touched by this committed registration lost their previous
	// availability observation. Discovery enrollment must not force every other
	// source to persist a fresh health transition on the next scan.
	for key := range seen {
		delete(s.healthChecks, key)
	}
	for key := range removed {
		delete(s.healthChecks, key)
	}
	return nil
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
		return sessioncapture.PublicCheckpoint(cp), nil
	}
	if err != nil {
		return cp, err
	}
	if err := json.Unmarshal([]byte(data), &cp); err != nil {
		return cp, errors.New("corrupt_checkpoint")
	}
	return sessioncapture.PublicCheckpoint(cp), nil
}

func (s *Store) Ingest(ctx context.Context, r Registration, previous sessioncapture.Checkpoint, batch sessioncapture.Batch, now time.Time) (finalErr error) {
	defer func() { finalErr = capacityError(finalErr) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ingest(ctx, r, previous, batch, now, nil)
}

// ingest requires the writer mutex. Health observations become visible only
// after the transaction that durably accepted their evidence commits.
func (s *Store) ingest(ctx context.Context, r Registration, previous sessioncapture.Checkpoint, batch sessioncapture.Batch, now time.Time, receipt *sessioncapture.Acknowledgement) error {
	previous = sessioncapture.PublicCheckpoint(previous)
	batch, err := sessioncapture.SanitizeBatch(batch)
	if err != nil {
		return err
	}
	needed, err := validateIngest(r, previous, batch)
	if err != nil {
		return err
	}
	if err := s.checkIngestSpace(ctx, s.db, needed); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	health, err := s.ingestTx(ctx, tx, r, previous, batch, now, receipt)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return capacityError(err)
	}
	s.rememberHealth(r.Key(), health, now)
	return nil
}

func validateIngest(r Registration, previous sessioncapture.Checkpoint, batch sessioncapture.Batch) (uint64, error) {
	if err := r.Validate(); err != nil {
		return 0, err
	}
	cp := batch.Checkpoint
	if cp.Version != 1 || cp.SourceKey != r.Key() || !sessionrecord.ValidDigest(cp.PrefixDigest) || cp.Offset < 0 || cp.Ordinal < 0 || cp.ParsedOffset < 0 || cp.ParsedOffset > cp.Offset {
		return 0, errors.New("invalid_candidate_checkpoint")
	}
	if cp.Epoch < previous.Epoch || cp.Epoch > previous.Epoch+1 || (cp.Epoch == previous.Epoch && (cp.Offset < previous.Offset || cp.Ordinal < previous.Ordinal)) {
		return 0, errors.New("checkpoint_regression")
	}
	var needed uint64
	baseOffset, baseOrdinal := previous.Offset, previous.Ordinal
	if cp.Epoch != previous.Epoch {
		baseOffset = 0
		baseOrdinal = 0
	}
	if previous.Generation != "" && cp.Epoch == previous.Epoch && cp.Generation != previous.Generation {
		return 0, errors.New("generation_conflict")
	}
	for i, item := range batch.Records {
		rec := item.Record
		if err := secretredact.Check(item.Raw); err != nil {
			return 0, err
		}
		if err := secretredact.Check([]byte(encode(rec))); err != nil {
			return 0, err
		}
		if err := item.Verify(); err != nil {
			return 0, err
		}
		if rec.Provider != r.Provider || rec.SourceNamespace != r.Namespace || rec.ConversationKey == nil || *rec.ConversationKey != sessionrecord.Conversation(r.Namespace, cp.ConversationID) || rec.SourceOrder == nil || rec.SourceOrder.StreamGeneration != cp.Generation {
			return 0, errors.New("batch_source_mismatch")
		}
		if rec.SourceOrder.Ordinal != baseOrdinal+int64(i) {
			return 0, errors.New("candidate_order_mismatch")
		}
		baseOffset += item.NativeLength()
		needed += uint64(len(item.Raw) + len(encode(rec))*2 + 4096)
	}
	if cp.Offset != baseOffset || cp.Ordinal != baseOrdinal+int64(len(batch.Records)) {
		return 0, errors.New("candidate_boundary_mismatch")
	}
	return needed, nil
}

func (s *Store) checkIngestSpace(ctx context.Context, q rowQuerier, needed uint64) error {
	if err := s.checkSpace(needed); err != nil {
		return err
	}
	if needed > 0 {
		var pages, size int64
		if err := q.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
			return err
		}
		if err := q.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
			return err
		}
		if pages*size >= s.options.MaxDatabaseBytes {
			return ErrCapacity
		}
	}
	return nil
}

// ingestTx writes records, checkpoint and optional receipt into its caller's
// transaction. It must not update the in-memory health cache before commit.
func (s *Store) ingestTx(ctx context.Context, tx *sql.Tx, r Registration, previous sessioncapture.Checkpoint, batch sessioncapture.Batch, now time.Time, receipt *sessioncapture.Acknowledgement) (string, error) {
	cp := batch.Checkpoint
	stored, err := checkpoint(ctx, tx, r.Key())
	if err != nil {
		return "", err
	}
	if stored != previous {
		return "", ErrCheckpoint
	}
	var active int
	if err := tx.QueryRowContext(ctx, "SELECT active FROM stream_locators WHERE stream_key=?", r.Key()).Scan(&active); err != nil {
		return "", err
	}
	if active != 1 {
		return "", errors.New("source_not_registered")
	}
	conversation := ""
	if cp.ConversationID != "" {
		conversation = sessionrecord.Conversation(r.Namespace, cp.ConversationID)
	}
	if err := checkSuppression(ctx, tx, r.Namespace, conversation, ""); err != nil {
		return "", err
	}
	if cp.Generation != "" {
		if _, err := tx.ExecContext(ctx, "INSERT INTO stream_generations VALUES (?,?,?) ON CONFLICT DO NOTHING", r.Key(), cp.Generation, cp.Epoch); err != nil {
			return "", err
		}
	}
	if cp.ConversationID != "" {
		if _, err := tx.ExecContext(ctx, "INSERT INTO sessions VALUES (?,?,?,'unknown') ON CONFLICT DO NOTHING", sessionrecord.Conversation(r.Namespace, cp.ConversationID), r.Namespace, cp.ConversationID); err != nil {
			return "", err
		}
	}
	for _, item := range batch.Records {
		rec := item.Record
		var suppressed int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM purge_tombstones WHERE namespace=? AND
   ((scope_kind='source' AND scope_key=?) OR (scope_kind='conversation' AND scope_key=?) OR (scope_kind='record' AND scope_key=?))`, r.Namespace, r.Namespace, *rec.ConversationKey, rec.RecordKey).Scan(&suppressed); err != nil {
			return "", err
		}
		if suppressed > 0 {
			return "", errors.New("capture_suppressed")
		}
		if err := insertContentPayload(ctx, tx, rec.SourceRevision, item.Raw); err != nil {
			return "", capacityError(err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO artifacts VALUES (?,?,?,?,?) ON CONFLICT DO NOTHING", rec.RecordKey, r.Namespace, rec.OriginScope.Kind, rec.OriginScope.Key, *rec.ConversationKey); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO artifact_versions VALUES (?,?,?) ON CONFLICT DO NOTHING", rec.RecordKey, rec.SourceRevision, stamp(rec.ObservedAt)); err != nil {
			return "", err
		}
		var sourceTime any
		if rec.SourceTime != nil {
			sourceTime = stamp(*rec.SourceTime)
		}
		if err := insertNormalizationPayload(ctx, tx, rec, sourceTime); err != nil {
			return "", capacityError(err)
		}
		result, err := tx.ExecContext(ctx, "INSERT INTO record_observations VALUES (?,?,?,?,?,?) ON CONFLICT DO NOTHING", r.Key(), cp.Generation, rec.SourceOrder.Ordinal, rec.RecordKey, rec.SourceRevision, stamp(now))
		if err != nil {
			return "", err
		}
		inserted, _ := result.RowsAffected()
		if inserted > 0 {
			var priorRevision string
			priorErr := tx.QueryRowContext(ctx, "SELECT source_revision FROM record_heads WHERE record_key=?", rec.RecordKey).Scan(&priorRevision)
			if priorErr != nil && priorErr != sql.ErrNoRows {
				return "", priorErr
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO record_heads VALUES (?,?,?) ON CONFLICT(record_key) DO UPDATE SET source_revision=excluded.source_revision,adapter_version=excluded.adapter_version WHERE record_heads.source_revision IS NOT excluded.source_revision OR record_heads.adapter_version IS NOT excluded.adapter_version`, rec.RecordKey, rec.SourceRevision, rec.AdapterVersion); err != nil {
				return "", err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO changes(change_kind,namespace,record_key,source_revision,payload_json) VALUES ('record',?,?,?,?)", r.Namespace, rec.RecordKey, rec.SourceRevision, encode(map[string]any{"stream": r.Key(), "generation": cp.Generation, "ordinal": rec.SourceOrder.Ordinal, "adapter_version": rec.AdapterVersion, "correction": priorErr == nil && priorRevision != rec.SourceRevision})); err != nil {
				return "", err
			}
			if rec.Kind == "title" && rec.Body.Text != "" {
				text := []rune(rec.Body.Text)
				if len(text) > 240 {
					text = text[:240]
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO session_titles(conversation_key,record_key,source_revision,adapter_version,title,stream_key,epoch,ordinal,ordering_ambiguous,origin)
 VALUES (?,?,?,?,?,?,?,?,0,'native')
 ON CONFLICT(conversation_key) DO UPDATE SET
 record_key=excluded.record_key,source_revision=excluded.source_revision,adapter_version=excluded.adapter_version,
 title=excluded.title,epoch=excluded.epoch,ordinal=excluded.ordinal,origin='native'
 WHERE session_titles.origin IN ('derived','continuation') OR (session_titles.stream_key=excluded.stream_key AND (excluded.epoch>session_titles.epoch OR
 (excluded.epoch=session_titles.epoch AND excluded.ordinal>=session_titles.ordinal)))`, *rec.ConversationKey, rec.RecordKey, rec.SourceRevision, rec.AdapterVersion, string(text), r.Key(), cp.Epoch, rec.SourceOrder.Ordinal); err != nil {
					return "", err
				}
				if _, err := tx.ExecContext(ctx, "UPDATE session_titles SET ordering_ambiguous=1 WHERE ordering_ambiguous=0 AND conversation_key=? AND stream_key!=?", *rec.ConversationKey, r.Key()); err != nil {
					return "", err
				}
			} else if title, ok := derivedTitle(rec); ok {
				// Earliest eligible evidence wins within a stream; a native title
				// record always replaces a derived projection.
				if _, err := tx.ExecContext(ctx, `INSERT INTO session_titles(conversation_key,record_key,source_revision,adapter_version,title,stream_key,epoch,ordinal,ordering_ambiguous,origin)
 VALUES (?,?,?,?,?,?,?,?,0,'derived')
 ON CONFLICT(conversation_key) DO UPDATE SET
 record_key=excluded.record_key,source_revision=excluded.source_revision,adapter_version=excluded.adapter_version,
 title=excluded.title,epoch=excluded.epoch,ordinal=excluded.ordinal,origin='derived'
 WHERE session_titles.origin='continuation' OR (session_titles.origin='derived' AND session_titles.stream_key=excluded.stream_key AND (excluded.epoch<session_titles.epoch OR
 (excluded.epoch=session_titles.epoch AND excluded.ordinal<session_titles.ordinal)))`, *rec.ConversationKey, rec.RecordKey, rec.SourceRevision, rec.AdapterVersion, title, r.Key(), cp.Epoch, rec.SourceOrder.Ordinal); err != nil {
					return "", err
				}
				if _, err := tx.ExecContext(ctx, "UPDATE session_titles SET ordering_ambiguous=1 WHERE ordering_ambiguous=0 AND conversation_key=? AND stream_key!=? AND origin='derived'", *rec.ConversationKey, r.Key()); err != nil {
					return "", err
				}
			}
			if err := updateActivity(ctx, tx, *rec.ConversationKey, r.Key(), cp.Epoch, rec); err != nil {
				return "", err
			}
		}
		var cwd string
		if raw, ok := rec.Extensions["native_cwd"]; ok && json.Unmarshal(raw, &cwd) == nil && cwd != "" && len(cwd) <= 4096 {
			if _, err := tx.ExecContext(ctx, "INSERT INTO session_projects VALUES (?,?,?,?) ON CONFLICT DO NOTHING", *rec.ConversationKey, cwd, rec.RecordKey, rec.SourceRevision); err != nil {
				return "", err
			}
		}
		var parent string
		if raw, ok := rec.Extensions["native_parent"]; ok && json.Unmarshal(raw, &parent) == nil && parent != "" {
			if err := continuationTitle(ctx, tx, *rec.ConversationKey, r.Namespace, parent, r.Key(), cp.Epoch, rec); err != nil {
				return "", err
			}
		}
		var fp string
		if raw, ok := rec.Extensions["native_file_path"]; ok && json.Unmarshal(raw, &fp) == nil && fp != "" {
			if err := commonProjectDir(ctx, tx, *rec.ConversationKey, filepath.Dir(fp), rec); err != nil {
				return "", err
			}
		}
		var nd string
		if raw, ok := rec.Extensions["native_dir"]; ok && json.Unmarshal(raw, &nd) == nil && nd != "" {
			if err := commonProjectDir(ctx, tx, *rec.ConversationKey, nd, rec); err != nil {
				return "", err
			}
		}
	}
	if batch.SourceActive && !batch.SourceActiveAt.IsZero() && cp.ConversationID != "" {
		// A hot source WAL is liveness evidence: the conversation is being
		// written right now. Project working at the write time; a quiet
		// snapshot or a stale timestamp replaces it with record evidence.
		conv := sessionrecord.Conversation(r.Namespace, cp.ConversationID)
		at := stamp(batch.SourceActiveAt)
		if _, err := tx.ExecContext(ctx, `UPDATE session_activity SET signal='working',signal_time=?,seen_time=? WHERE conversation_key=? AND (signal IS NOT 'working' OR signal_time IS NOT ? OR seen_time IS NOT ?)`, at, at, conv, at, at); err != nil {
			return "", err
		}
	}
	for _, gap := range batch.Gaps {
		result, err := tx.ExecContext(ctx, "INSERT INTO capture_gaps VALUES (?,?,?,?,?) ON CONFLICT DO NOTHING", r.Key(), gap.Generation, gap.Offset, gap.Code, stamp(now))
		if err != nil {
			return "", err
		}
		n, _ := result.RowsAffected()
		if n > 0 {
			if _, err := tx.ExecContext(ctx, "INSERT INTO changes(change_kind,namespace,payload_json) VALUES ('gap',?,?)", r.Namespace, encode(map[string]any{"stream": r.Key(), "generation": gap.Generation, "ordinal": gap.Ordinal, "offset": gap.Offset, "code": gap.Code})); err != nil {
				return "", err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ingest_checkpoints VALUES (?,?,?,?,?,?,?) ON CONFLICT(stream_key) DO UPDATE SET generation=excluded.generation,captured_offset=excluded.captured_offset,parsed_offset=excluded.parsed_offset,ordinal=excluded.ordinal,checkpoint_json=excluded.checkpoint_json,has_gaps=excluded.has_gaps WHERE (ingest_checkpoints.generation,ingest_checkpoints.captured_offset,ingest_checkpoints.parsed_offset,ingest_checkpoints.ordinal,ingest_checkpoints.checkpoint_json,ingest_checkpoints.has_gaps) IS NOT (excluded.generation,excluded.captured_offset,excluded.parsed_offset,excluded.ordinal,excluded.checkpoint_json,excluded.has_gaps)`, r.Key(), cp.Generation, cp.Offset, cp.ParsedOffset, cp.Ordinal, encode(cp), cp.HasGaps); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE stream_locators SET conversation_id=? WHERE stream_key=? AND conversation_id IS NOT ?", cp.ConversationID, r.Key(), cp.ConversationID); err != nil {
		return "", err
	}
	health := "available"
	if batch.Pending && !batch.SourceActive {
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
		return "", err
	}
	capabilities := encode(sessioncapture.Probe(r.Provider, cp.ProviderVersion))
	if _, err := tx.ExecContext(ctx, "UPDATE sources SET capabilities_json=?,adapter_version=? WHERE namespace=? AND (capabilities_json IS NOT ? OR adapter_version IS NOT ?)", capabilities, sessioncapture.AdapterVersion, r.Namespace, capabilities, sessioncapture.AdapterVersion); err != nil {
		return "", err
	}
	if receipt != nil {
		if _, err := tx.ExecContext(ctx, "INSERT INTO capture_receipts VALUES (?,?,?,?,?)", receipt.Destination, receipt.BatchID, receipt.SourceKey, encode(receipt), stamp(now)); err != nil {
			return "", err
		}
	}
	return health, nil
}

// activitySignal extracts a conversation activity signal from one record.
// lifecycle_observation state (working/idle) comes from the provider; an
// input-request tool call (AskUserQuestion, request_user_input) signals input,
// whether it arrives as a standalone record or a message part. Providers
// without lifecycle records derive turn state from the newest record's shape:
// a user tail or a tool call in flight is working evidence, a plain agent
// reply or a recap is idle evidence. Everything else carries no signal.
func activitySignal(rec sessionrecord.Record) string {
	if rec.Kind == "lifecycle_observation" && (rec.Body.State == "working" || rec.Body.State == "idle") {
		return rec.Body.State
	}
	input, tool := false, false
	for _, p := range rec.Body.Parts {
		if p.Name == "AskUserQuestion" || p.Name == "request_user_input" {
			input = true
		}
		if p.Type == "tool_call" {
			tool = true
		}
	}
	if input {
		return "input"
	}
	if rec.Provider == sessioncapture.Claude || rec.Provider == sessioncapture.Devin {
		switch rec.Kind {
		case "message":
			if rec.Role != nil && *rec.Role == "user" || tool {
				return "working"
			}
			return "idle"
		case "tool_call":
			return "working"
		case "native_recap":
			return "idle"
		}
	}
	return ""
}

// updateActivity projects the latest signal and newest observed position per
// conversation. Only records on the winning stream move the projection; records
// on other streams mark ordering ambiguous rather than inventing order.
func updateActivity(ctx context.Context, tx *sql.Tx, conv, stream string, epoch int64, rec sessionrecord.Record) error {
	ord := rec.SourceOrder.Ordinal
	var sourceTime any
	if rec.SourceTime != nil {
		sourceTime = stamp(*rec.SourceTime)
	}
	sig := activitySignal(rec)
	var curStream, curSignal string
	var sigEpoch, sigOrd, seenEpoch, seenOrd int64
	var sigTime, seenTime any
	err := tx.QueryRowContext(ctx, `SELECT stream_key,signal,signal_epoch,signal_ordinal,signal_time,seen_epoch,seen_ordinal,seen_time
 FROM session_activity WHERE conversation_key=?`, conv).Scan(&curStream, &curSignal, &sigEpoch, &sigOrd, &sigTime, &seenEpoch, &seenOrd, &seenTime)
	if err == sql.ErrNoRows {
		signal := "none"
		var st any
		se, so := int64(0), int64(0)
		if sig != "" {
			signal, se, so, st = sig, epoch, ord, sourceTime
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO session_activity VALUES (?,?,?,?,?,?,?,?,?,0)",
			conv, stream, signal, se, so, st, epoch, ord, sourceTime)
		return err
	}
	if err != nil {
		return err
	}
	if curStream != stream {
		_, err := tx.ExecContext(ctx, "UPDATE session_activity SET ordering_ambiguous=1 WHERE ordering_ambiguous=0 AND conversation_key=?", conv)
		return err
	}
	if sig != "" && (curSignal == "none" || epoch > sigEpoch || (epoch == sigEpoch && ord >= sigOrd)) {
		curSignal, sigEpoch, sigOrd, sigTime = sig, epoch, ord, sourceTime
	}
	if epoch > seenEpoch || (epoch == seenEpoch && ord > seenOrd) {
		seenEpoch, seenOrd = epoch, ord
		if sourceTime != nil {
			seenTime = sourceTime
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE session_activity SET signal=?,signal_epoch=?,signal_ordinal=?,signal_time=?,seen_epoch=?,seen_ordinal=?,seen_time=?
 WHERE conversation_key=? AND (signal,signal_epoch,signal_ordinal,signal_time,seen_epoch,seen_ordinal,seen_time) IS NOT (?,?,?,?,?,?,?)`, curSignal, sigEpoch, sigOrd, sigTime, seenEpoch, seenOrd, seenTime, conv, curSignal, sigEpoch, sigOrd, sigTime, seenEpoch, seenOrd, seenTime)
	return err
}

// continuationTitle labels a compaction-resume session from its parent
// thread's title. It never displaces stronger evidence: origin 'continuation'
// sits below derived and native titles, and yields to either on arrival.
func continuationTitle(ctx context.Context, tx *sql.Tx, conv, namespace, parentID, stream string, epoch int64, rec sessionrecord.Record) error {
	var title string
	err := tx.QueryRowContext(ctx, `SELECT t.title FROM sessions p JOIN session_titles t USING(conversation_key)
 WHERE p.namespace=? AND p.native_id=? AND t.title!=''`, namespace, parentID).Scan(&title)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	label := []rune("Continuation of " + secretredact.Text(title, nil))
	if len(label) > 240 {
		label = label[:240]
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_titles(conversation_key,record_key,source_revision,adapter_version,title,stream_key,epoch,ordinal,ordering_ambiguous,origin)
 VALUES (?,?,?,?,?,?,?,?,0,'continuation')`, conv, rec.RecordKey, rec.SourceRevision, rec.AdapterVersion, string(label), stream, epoch, rec.SourceOrder.Ordinal)
	return err
}

// commonProjectDir records a file-evidence project per conversation: the
// nearest ancestor directory containing .git, falling back to the touched
// directory itself. Each distinct root gets a bounded row, so sessions that
// genuinely span projects appear under each. Explicit native_cwd rows are
// unaffected.
func commonProjectDir(ctx context.Context, tx *sql.Tx, conv, dir string, rec sessionrecord.Record) error {
	dir = strings.TrimSuffix(dir, "/")
	if !strings.HasPrefix(dir, "/") || len(dir) > 4096 {
		return nil
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM session_projects WHERE conversation_key=?", conv).Scan(&n); err != nil {
		return err
	}
	root := projectRoot(dir)
	if root == "" || n >= 8 {
		return nil
	}
	_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO session_projects VALUES (?,?,?,?)", conv, secretredact.Text(root, nil), rec.RecordKey, rec.SourceRevision)
	return err
}

// projectRoot walks up to the nearest git working tree. Paths outside one
// (screenshots, scratch files, shared home directories) are not projects.
func projectRoot(dir string) string {
	if !strings.HasPrefix(dir, "/") || len(dir) > 4096 {
		return ""
	}
	for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
	}
	return ""
}

// derivedTitle projects the first substantive user message into a display
// title for conversations whose provider never writes a native title record.
// Injected context blocks and history/caveat preambles are not eligible; the
// projection is labeled origin='derived' and keeps its record provenance.
func derivedTitle(rec sessionrecord.Record) (string, bool) {
	if rec.Kind != "message" || rec.Role == nil || *rec.Role != "user" {
		return "", false
	}
	text := strings.TrimSpace(secretredact.Text(rec.Body.Text, nil))
	if strings.HasPrefix(text, "The following is the Codex agent history") {
		// Approval-assessment rollouts wrap the assessed work in an APPROVAL
		// REQUEST block whose "justification" describes it in natural language.
		text = approvalJustification(text)
	} else if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	if text == "" || strings.HasPrefix(text, "<") ||
		strings.HasPrefix(text, "The following is the Codex agent history") ||
		strings.HasPrefix(text, "Caveat: The messages below") {
		return "", false
	}
	runes := []rune(text)
	if len(runes) > 240 {
		runes = runes[:240]
	}
	return string(runes), true
}

// approvalJustification recovers the described action from a Codex
// approval-assessment preamble. The message is text, not JSON, so the quoted
// field is decoded directly from its last occurrence.
func approvalJustification(text string) string {
	const mark = `"justification":`
	i := strings.LastIndex(text, mark)
	if i < 0 {
		return ""
	}
	var just string
	if json.NewDecoder(strings.NewReader(text[i+len(mark):])).Decode(&just) != nil {
		return ""
	}
	return just
}

// reprojectTitles backfills title rows for sessions captured before the
// projection existed. It runs at every Open and only visits conversations
// without any title row, so it is a no-op once projections are complete.
// Pass 1 recovers native titles from opaque ai-title records whose text lives
// in the retained original bytes; pass 2 derives titles from user messages.
func (s *Store) reprojectTitles(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	type pick struct {
		recordKey, revision, adapter, stream, title string
		epoch, ordinal                              int64
		streams                                     map[string]bool
	}
	// Conversations without a native title are eligible: absent rows are
	// inserted, derived projections are upgraded in place.
	eligible := ` LEFT JOIN session_titles t ON t.conversation_key=a.conversation_key`
	position := `FROM normalizations n
 JOIN artifacts a USING(record_key)
 JOIN record_observations o ON o.record_key=n.record_key AND o.source_revision=n.source_revision
 JOIN stream_generations g ON g.stream_key=o.stream_key AND g.generation=o.generation`
	// Latest ai-title evidence wins within a conversation, matching ingest order.
	rows, err := tx.QueryContext(ctx, `SELECT a.conversation_key,n.record_key,n.source_revision,n.adapter_version,
 o.stream_key,g.epoch,o.ordinal,c.original_bytes
 `+position+eligible+` JOIN content_objects c ON c.digest=n.source_revision
 WHERE n.kind='opaque_record' AND json_extract(n.envelope_json,'$.native_kind')='ai-title'
 AND (t.origin IS NULL OR t.origin IN ('derived','continuation'))
 ORDER BY a.conversation_key,g.epoch DESC,o.ordinal DESC,o.stream_key`)
	if err != nil {
		return err
	}
	native := map[string]*pick{}
	for rows.Next() {
		var conv, key, revision, adapter, stream string
		var epoch, ordinal int64
		var raw []byte
		if err := rows.Scan(&conv, &key, &revision, &adapter, &stream, &epoch, &ordinal, &raw); err != nil {
			rows.Close()
			return err
		}
		var line struct {
			AiTitle string `json:"aiTitle"`
			Title   string `json:"title"`
		}
		if json.Unmarshal(secretredact.JSON(raw, nil), &line) != nil {
			continue
		}
		title := line.AiTitle
		if title == "" {
			title = line.Title
		}
		if title == "" {
			continue
		}
		if runes := []rune(title); len(runes) > 240 {
			title = string(runes[:240])
		}
		p := native[conv]
		if p == nil {
			p = &pick{streams: map[string]bool{}}
			native[conv] = p
		}
		p.streams[stream] = true
		if p.recordKey == "" {
			p.recordKey, p.revision, p.adapter = key, revision, adapter
			p.stream, p.epoch, p.ordinal, p.title = stream, epoch, ordinal, title
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for conv, p := range native {
		ambiguous := 0
		if len(p.streams) > 1 {
			ambiguous = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_titles(conversation_key,record_key,source_revision,adapter_version,title,stream_key,epoch,ordinal,ordering_ambiguous,origin)
 VALUES (?,?,?,?,?,?,?,?,?,'native')
 ON CONFLICT(conversation_key) DO UPDATE SET
 record_key=excluded.record_key,source_revision=excluded.source_revision,adapter_version=excluded.adapter_version,
 title=excluded.title,stream_key=excluded.stream_key,epoch=excluded.epoch,ordinal=excluded.ordinal,
 ordering_ambiguous=excluded.ordering_ambiguous,origin='native'
 WHERE session_titles.origin IN ('derived','continuation')`, conv, p.recordKey, p.revision, p.adapter, p.title, p.stream, p.epoch, p.ordinal, ambiguous); err != nil {
			return err
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT a.conversation_key,n.record_key,n.source_revision,n.adapter_version,
 o.stream_key,g.epoch,o.ordinal,n.envelope_json
 `+position+`
 WHERE n.kind='message'
 AND a.conversation_key NOT IN (SELECT conversation_key FROM session_titles)
 ORDER BY a.conversation_key,g.epoch,o.ordinal,o.stream_key`)
	if err != nil {
		return err
	}
	derived := map[string]*pick{}
	for rows.Next() {
		var conv, key, revision, adapter, stream, envelope string
		var epoch, ordinal int64
		if err := rows.Scan(&conv, &key, &revision, &adapter, &stream, &epoch, &ordinal, &envelope); err != nil {
			rows.Close()
			return err
		}
		p := derived[conv]
		if p == nil {
			p = &pick{streams: map[string]bool{}}
			derived[conv] = p
		}
		rec, err := sessionrecord.Decode(secretredact.JSON([]byte(envelope), nil))
		if err != nil {
			continue
		}
		title, ok := derivedTitle(rec)
		if !ok {
			continue
		}
		p.streams[stream] = true
		if p.recordKey == "" {
			p.recordKey, p.revision, p.adapter = key, revision, adapter
			p.stream, p.epoch, p.ordinal, p.title = stream, epoch, ordinal, title
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for conv, p := range derived {
		if p.recordKey == "" {
			continue
		}
		ambiguous := 0
		if len(p.streams) > 1 {
			ambiguous = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_titles(conversation_key,record_key,source_revision,adapter_version,title,stream_key,epoch,ordinal,ordering_ambiguous,origin)
 VALUES (?,?,?,?,?,?,?,?,?,'derived')`, conv, p.recordKey, p.revision, p.adapter, p.title, p.stream, p.epoch, p.ordinal, ambiguous); err != nil {
			return err
		}
	}
	// Compaction-resume sessions with no stronger evidence inherit a label
	// from the parent thread named in their session_meta. The parent pointer
	// is read from retained raw bytes so pre-extension records qualify.
	rows, err = tx.QueryContext(ctx, `SELECT a.conversation_key,n.record_key,n.source_revision,n.adapter_version,
 json_extract(c.original_bytes,'$.payload.session_id'),o.stream_key,g.epoch,o.ordinal,t2.title
 `+position+` JOIN content_objects c ON c.digest=n.source_revision
 JOIN sessions sp ON sp.namespace=a.namespace AND sp.native_id=json_extract(c.original_bytes,'$.payload.session_id')
 JOIN session_titles t2 ON t2.conversation_key=sp.conversation_key
 WHERE n.kind='opaque_record' AND json_extract(n.envelope_json,'$.native_kind')='session_meta'
 AND json_extract(c.original_bytes,'$.payload.session_id')!=json_extract(c.original_bytes,'$.payload.id')
 AND t2.title!='' AND a.conversation_key NOT IN (SELECT conversation_key FROM session_titles)
 ORDER BY a.conversation_key,g.epoch,o.ordinal,o.stream_key`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var conv, key, revision, adapter, parent, stream, title string
		var epoch, ordinal int64
		if err := rows.Scan(&conv, &key, &revision, &adapter, &parent, &stream, &epoch, &ordinal, &title); err != nil {
			rows.Close()
			return err
		}
		label := []rune("Continuation of " + secretredact.Text(title, nil))
		if len(label) > 240 {
			label = label[:240]
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_titles(conversation_key,record_key,source_revision,adapter_version,title,stream_key,epoch,ordinal,ordering_ambiguous,origin)
 VALUES (?,?,?,?,?,?,?,?,0,'continuation')`, conv, key, revision, adapter, string(label), stream, epoch, ordinal); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	// File-evidence projects: the nearest git working tree of each native
	// file path, for conversations with no explicit project evidence.
	rows, err = tx.QueryContext(ctx, `SELECT a.conversation_key,n.record_key,n.source_revision,
 coalesce(json_extract(c.original_bytes,'$.payload.content.rawInput.file_path'),''),
 coalesce(json_extract(c.original_bytes,'$.payload.content.rawInput.path'),''),
 coalesce(json_extract(c.original_bytes,'$.payload.content.rawInput.workdir'),'')
 `+position+` JOIN content_objects c ON c.digest=n.source_revision
 JOIN streams st ON st.stream_key=o.stream_key JOIN sources src ON src.namespace=st.namespace
 WHERE src.provider='devin' AND json_extract(n.envelope_json,'$.native_kind')='tool_call'
 ORDER BY a.conversation_key`)
	if err != nil {
		return err
	}
	type pathPick struct {
		key, revision string
	}
	projects := map[string]map[string]pathPick{}
	for rows.Next() {
		var conv, key, revision, fp, dp, wd string
		if err := rows.Scan(&conv, &key, &revision, &fp, &dp, &wd); err != nil {
			rows.Close()
			return err
		}
		dirs := []string{}
		if fp != "" {
			dirs = append(dirs, filepath.Dir(strings.TrimSuffix(fp, "/")))
		}
		if dp != "" {
			dirs = append(dirs, strings.TrimSuffix(dp, "/"))
		}
		if wd != "" {
			dirs = append(dirs, strings.TrimSuffix(wd, "/"))
		}
		set := projects[conv]
		if set == nil {
			set = map[string]pathPick{}
			projects[conv] = set
		}
		for _, dir := range dirs {
			root := projectRoot(dir)
			if root == "" {
				continue
			}
			if len(set) < 8 {
				set[root] = pathPick{key: key, revision: revision}
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for conv, set := range projects {
		for root, p := range set {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO session_projects VALUES (?,?,?,?)", conv, secretredact.Text(root, nil), p.key, p.revision); err != nil {
				return err
			}
		}
	}
	// Activity projections: newest observed position plus the latest signal per
	// conversation. Envelopes are decoded only for signal-carrying kinds.
	// Conversations projected before derived signals existed hold 'none' for
	// providers without lifecycle records; clear those rows so they re-project.
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_activity WHERE signal='none' AND conversation_key IN
 (SELECT s.conversation_key FROM sessions s JOIN sources src USING(namespace)
  WHERE src.provider IN ('claude_code','devin'))`); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT a.conversation_key,o.stream_key,g.epoch,o.ordinal,n.source_time,n.kind,
 json_extract(n.envelope_json,'$.body.state'),n.envelope_json
 `+position+`
 WHERE a.conversation_key NOT IN (SELECT conversation_key FROM session_activity)
 ORDER BY a.conversation_key,g.epoch DESC,o.ordinal DESC,o.stream_key`)
	if err != nil {
		return err
	}
	type activity struct {
		stream       string
		seenE, seenO int64
		seenT        any
		signal       string
		sigE, sigO   int64
		sigT         any
		sigFound     bool
		streams      map[string]bool
	}
	acts := map[string]*activity{}
	for rows.Next() {
		var conv, stream, kind, envelope string
		var epoch, ordinal int64
		var sourceTime, bodyState any
		if err := rows.Scan(&conv, &stream, &epoch, &ordinal, &sourceTime, &kind, &bodyState, &envelope); err != nil {
			rows.Close()
			return err
		}
		a := acts[conv]
		if a == nil {
			a = &activity{stream: stream, seenE: epoch, seenO: ordinal, seenT: sourceTime, streams: map[string]bool{}}
			acts[conv] = a
		}
		a.streams[stream] = true
		if a.sigFound {
			continue
		}
		sig := ""
		if kind == "lifecycle_observation" {
			if s, ok := bodyState.(string); ok && (s == "working" || s == "idle") {
				sig = s
			}
		} else if kind == "tool_call" || kind == "message" || kind == "native_recap" {
			if rec, err := sessionrecord.Decode(secretredact.JSON([]byte(envelope), nil)); err == nil {
				sig = activitySignal(rec)
			}
		}
		if sig != "" {
			a.signal, a.sigE, a.sigO, a.sigT, a.sigFound = sig, epoch, ordinal, sourceTime, true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for conv, a := range acts {
		signal := a.signal
		if !a.sigFound {
			signal = "none"
		}
		ambiguous := 0
		if len(a.streams) > 1 {
			ambiguous = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_activity(conversation_key,stream_key,signal,signal_epoch,signal_ordinal,signal_time,seen_epoch,seen_ordinal,seen_time,ordering_ambiguous)
 VALUES (?,?,?,?,?,?,?,?,?,?)`, conv, a.stream, signal, a.sigE, a.sigO, a.sigT, a.seenE, a.seenO, a.seenT, ambiguous); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	if _, err := tx.ExecContext(ctx, "UPDATE stream_locators SET health=?,checked_at=? WHERE stream_key=? AND (health IS NOT ? OR checked_at IS NOT ?)", health, stamp(now), key, health, stamp(now)); err != nil {
		return err
	}
	if previous != health {
		_, err := tx.ExecContext(ctx, "INSERT INTO changes(change_kind,namespace,payload_json) VALUES ('source',?,?)", namespace, encode(map[string]string{"stream": key, "health": health}))
		return err
	}
	return nil
}

// Persist periodic health heartbeats at most once a minute. Transitions remain
// immediate; status reads use the latest successful in-memory check. This avoids
// one FULL-synchronous SQLite write transaction per idle source per poll.
const healthHeartbeat = time.Minute

type healthCheck struct {
	health      string
	checkedAt   time.Time
	persistedAt time.Time
}

func (s *Store) rememberHealth(key, health string, now time.Time) {
	if s.healthChecks == nil {
		s.healthChecks = make(map[string]healthCheck)
	}
	s.healthChecks[key] = healthCheck{health: health, checkedAt: now, persistedAt: now}
}

func (s *Store) SetHealth(ctx context.Context, r Registration, health string, now time.Time) error {
	return s.observeHealth(ctx, r, health, now, false)
}

// ObserveHealth keeps successful checks fresh in memory and commits transitions
// immediately. Unchanged periodic heartbeats are grouped by FlushHealth, which
// the collector calls at the end of each scan, including a canceled scan.
func (s *Store) ObserveHealth(ctx context.Context, r Registration, health string, now time.Time) error {
	return s.observeHealth(ctx, r, health, now, true)
}

func (s *Store) observeHealth(ctx context.Context, r Registration, health string, now time.Time, batchHeartbeat bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if last, ok := s.healthChecks[r.Key()]; ok && last.health == health && !now.Before(last.persistedAt) && (batchHeartbeat || now.Sub(last.persistedAt) < healthHeartbeat) {
		if now.After(last.checkedAt) {
			last.checkedAt = now
		}
		s.healthChecks[r.Key()] = last
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setHealth(ctx, tx, r.Key(), r.Namespace, health, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.rememberHealth(r.Key(), health, now)
	return nil
}

// FlushHealth persists all due unchanged heartbeats in one transaction. Failed
// commits leave their durable timestamps unchanged and observations retryable.
// Backups and restart expose only committed timestamps, as with SetHealth.
func (s *Store) FlushHealth(ctx context.Context) error {
	_, err := s.FlushHealthBatch(ctx)
	return err
}

// HealthHeartbeatError identifies checks whose grouped heartbeat did not
// commit. Keys are for scoped status reporting; Error never exposes source
// identities or underlying SQLite details. Unwrap preserves error matching.
type HealthHeartbeatError struct {
	Keys []string
	Err  error
}

func (e *HealthHeartbeatError) Error() string { return "health_heartbeat_failed" }
func (e *HealthHeartbeatError) Unwrap() error { return e.Err }

// FlushHealthBatch reports the number of heartbeats committed together. Zero
// means no write transaction was needed, or the transaction failed.
func (s *Store) FlushHealthBatch(ctx context.Context) (count int, finalErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []string
	for key, check := range s.healthChecks {
		if check.checkedAt.Sub(check.persistedAt) >= healthHeartbeat {
			due = append(due, key)
		}
	}
	defer func() {
		if finalErr != nil && len(due) > 0 {
			finalErr = &HealthHeartbeatError{Keys: due, Err: finalErr}
		}
	}()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(due) == 0 {
		return 0, nil
	}
	slices.Sort(due)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, key := range due {
		check := s.healthChecks[key]
		if _, err := tx.ExecContext(ctx, "UPDATE stream_locators SET checked_at=? WHERE stream_key=? AND health=?", stamp(check.checkedAt), key, check.health); err != nil {
			return 0, capacityError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, capacityError(err)
	}
	for _, key := range due {
		check := s.healthChecks[key]
		check.persistedAt = check.checkedAt
		s.healthChecks[key] = check
	}
	return len(due), nil
}

func (s *Store) Stats(ctx context.Context, namespaces []string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats(ctx, namespaces)
}

// CROSS JOIN fixes the loop order: scope artifacts through its covering index,
// then count their versions through the version primary key. Without this,
// SQLite can scan every version and fetch an artifact table page for each one.
const scopedVersionCountSQL = "SELECT count(*) FROM artifacts a CROSS JOIN artifact_versions v USING(record_key) WHERE a.namespace IN ("

func (s *Store) recordVersionCount(ctx context.Context, in string, args []any) (int64, error) {
	// Check persisted sources, including retired namespaces that retain history.
	// An empty scope produces IN (NULL), which must never grant the full count.
	var allSources bool
	if err := s.db.QueryRowContext(ctx, "SELECT NOT EXISTS(SELECT 1 FROM sources WHERE (namespace IN ("+in+")) IS NOT TRUE)", args...).Scan(&allSources); err != nil {
		return 0, err
	}
	query := scopedVersionCountSQL + in + ")"
	if allSources {
		// SQLite's direct count walks compact index pages without a join or a
		// row-by-row aggregate. It is safe only when scope covers all history.
		query, args = "SELECT count(*) FROM artifact_versions", nil
	}
	var count int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

func (s *Store) stats(ctx context.Context, namespaces []string) (map[string]any, error) {
	in, args := scopeSQL(namespaces)
	var sessions int64
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE namespace IN ("+in+")", args...).Scan(&sessions); err != nil {
		return nil, err
	}
	versions, err := s.recordVersionCount(ctx, in, args)
	if err != nil {
		return nil, err
	}
	boundary, err := s.boundary()
	if err != nil {
		return nil, err
	}
	return map[string]any{"archive_instance": s.instance, "schema_version": SchemaVersion, "contract_version": 1, "boundary": boundary, "sessions": sessions, "record_versions": versions, "retrieval": "unavailable", "surface_activation": "unsupported"}, nil
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
