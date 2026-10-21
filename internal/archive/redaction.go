package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// RedactionReceipt describes one committed content removal. It contains only
// archive and target metadata; it never includes native bytes or projections.
type RedactionReceipt struct {
	Version         int    `json:"version"`
	ArchiveInstance string `json:"archive_instance"`
	Namespace       string `json:"namespace"`
	ScopeKind       string `json:"scope_kind"`
	ScopeKey        string `json:"scope_key"`
	Target          string `json:"target"`
	Sequence        int64  `json:"sequence"`
	RecordsRemoved  int64  `json:"records_removed"`
	VersionsRemoved int64  `json:"versions_removed"`
	ObjectsRemoved  int64  `json:"objects_removed"`
	BytesRemoved    int64  `json:"bytes_removed"`
	RedactedAt      string `json:"redacted_at"`
}

// Redact withdraws a source, conversation or record from this archive. Identity
// rows and old change hashes remain for attribution; raw bytes, normalized
// envelopes and derived projections are removed in one transaction.
func (s *Store) Redact(ctx context.Context, namespace, scopeKind, scopeKey string) (RedactionReceipt, error) {
	if namespace == "" || scopeKey == "" {
		return RedactionReceipt{}, errors.New("redaction_target_required")
	}
	if scopeKind != "source" && scopeKind != "conversation" && scopeKind != "record" {
		return RedactionReceipt{}, errors.New("invalid_redaction_scope")
	}
	if scopeKind == "source" && scopeKey != namespace {
		return RedactionReceipt{}, errors.New("redaction_scope_mismatch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// secure_delete overwrites deleted cells in the live SQLite database. WAL,
	// older backups and physical media remain outside this logical guarantee.
	if _, err := s.db.ExecContext(ctx, "PRAGMA secure_delete=ON"); err != nil {
		return RedactionReceipt{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RedactionReceipt{}, err
	}
	defer tx.Rollback()
	var stored string
	err = tx.QueryRowContext(ctx, "SELECT receipt_json FROM redaction_receipts WHERE namespace=? AND scope_kind=? AND scope_key=?", namespace, scopeKind, scopeKey).Scan(&stored)
	if err == nil {
		var receipt RedactionReceipt
		if json.Unmarshal([]byte(stored), &receipt) != nil || receipt.Version != 1 || receipt.Namespace != namespace || receipt.ScopeKind != scopeKind || receipt.ScopeKey != scopeKey {
			return RedactionReceipt{}, errors.New("corrupt_redaction_receipt")
		}
		return receipt, nil
	}
	if err != sql.ErrNoRows {
		return RedactionReceipt{}, err
	}
	var exists int
	var query string
	switch scopeKind {
	case "source":
		query = "SELECT count(*) FROM sources WHERE namespace=?"
	case "conversation":
		query = "SELECT count(*) FROM sessions WHERE namespace=? AND conversation_key=?"
	case "record":
		query = "SELECT count(*) FROM artifacts WHERE namespace=? AND record_key=?"
	}
	if scopeKind == "source" {
		err = tx.QueryRowContext(ctx, query, namespace).Scan(&exists)
	} else {
		err = tx.QueryRowContext(ctx, query, namespace, scopeKey).Scan(&exists)
	}
	if err != nil {
		return RedactionReceipt{}, err
	}
	if exists != 1 {
		return RedactionReceipt{}, errors.New("redaction_target_unavailable")
	}
	// The target table and digest table are transactional and connection-local.
	// They keep the exact deletion set stable while dependent rows disappear.
	if _, err := tx.ExecContext(ctx, "CREATE TEMP TABLE redaction_targets(record_key TEXT PRIMARY KEY)"); err != nil {
		return RedactionReceipt{}, err
	}
	targetSQL := "INSERT INTO redaction_targets SELECT record_key FROM artifacts WHERE namespace=?"
	args := []any{namespace}
	switch scopeKind {
	case "conversation":
		targetSQL += " AND conversation_key=?"
		args = append(args, scopeKey)
	case "record":
		targetSQL += " AND record_key=?"
		args = append(args, scopeKey)
	}
	if _, err := tx.ExecContext(ctx, targetSQL, args...); err != nil {
		return RedactionReceipt{}, err
	}
	receipt := RedactionReceipt{Version: 1, ArchiveInstance: s.instance, Namespace: namespace, ScopeKind: scopeKind, ScopeKey: scopeKey, Target: scopeKey, RedactedAt: stamp(time.Now().UTC())}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM redaction_targets").Scan(&receipt.RecordsRemoved); err != nil {
		return RedactionReceipt{}, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM artifact_versions WHERE record_key IN (SELECT record_key FROM redaction_targets)").Scan(&receipt.VersionsRemoved); err != nil {
		return RedactionReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, "CREATE TEMP TABLE redaction_digests(digest TEXT PRIMARY KEY)"); err != nil {
		return RedactionReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO redaction_digests SELECT DISTINCT source_revision FROM artifact_versions WHERE record_key IN (SELECT record_key FROM redaction_targets)"); err != nil {
		return RedactionReceipt{}, err
	}
	deletions := []string{
		"DELETE FROM session_titles WHERE record_key IN (SELECT record_key FROM redaction_targets)",
		"DELETE FROM session_projects WHERE record_key IN (SELECT record_key FROM redaction_targets)",
		"DELETE FROM session_activity WHERE conversation_key IN (SELECT DISTINCT conversation_key FROM artifacts WHERE record_key IN (SELECT record_key FROM redaction_targets))",
		"DELETE FROM record_heads WHERE record_key IN (SELECT record_key FROM redaction_targets)",
		"DELETE FROM record_observations WHERE record_key IN (SELECT record_key FROM redaction_targets)",
		"DELETE FROM normalizations WHERE record_key IN (SELECT record_key FROM redaction_targets)",
		"DELETE FROM artifact_versions WHERE record_key IN (SELECT record_key FROM redaction_targets)",
	}
	for _, statement := range deletions {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return RedactionReceipt{}, err
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(byte_length),0) FROM content_objects
 WHERE digest IN (SELECT digest FROM redaction_digests)
 AND NOT EXISTS(SELECT 1 FROM artifact_versions WHERE source_revision=content_objects.digest)`).Scan(&receipt.ObjectsRemoved, &receipt.BytesRemoved); err != nil {
		return RedactionReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM content_objects WHERE digest IN (SELECT digest FROM redaction_digests)
 AND NOT EXISTS(SELECT 1 FROM artifact_versions WHERE source_revision=content_objects.digest)`); err != nil {
		return RedactionReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO purge_tombstones(namespace,scope_kind,scope_key,revision,suppressed_at) VALUES(?,?,?,1,?)", namespace, scopeKind, scopeKey, receipt.RedactedAt); err != nil {
		return RedactionReceipt{}, err
	}
	change, err := tx.ExecContext(ctx, "INSERT INTO changes(change_kind,namespace,payload_json) VALUES('withdrawal',?,?)", namespace, encode(map[string]string{"reason": "redacted", "scope_kind": scopeKind, "scope_key": scopeKey}))
	if err != nil {
		return RedactionReceipt{}, err
	}
	receipt.Sequence, err = change.LastInsertId()
	if err != nil {
		return RedactionReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO redaction_receipts(namespace,scope_kind,scope_key,receipt_json) VALUES(?,?,?,?)", namespace, scopeKind, scopeKey, encode(receipt)); err != nil {
		return RedactionReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE redaction_digests"); err != nil {
		return RedactionReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE redaction_targets"); err != nil {
		return RedactionReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return RedactionReceipt{}, err
	}
	return receipt, nil
}
