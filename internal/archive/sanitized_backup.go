package archive

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/algorhythmic/skald/internal/secretredact"
	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
	"strings"
)

type backupDDL struct{ kind, name, sql string }

func quoteIdentifier(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// logicalBackup never copies source pages, freelists, WAL or unredacted payloads
// into a destination. It streams a consistent read snapshot into a fresh DB,
// remapping sanitized content references before inserting each row. Heap state
// contains only digest mappings, not a second archive-sized payload copy.
func logicalBackup(ctx context.Context, source *sql.DB, path string) (failure error) {
	defer func() {
		if recover() != nil {
			failure = errors.New("sanitized_backup_failed")
		}
		if failure != nil {
			failure = errors.New("sanitized_backup_failed")
		}
	}()
	read, err := source.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer read.Rollback()
	var version int
	if err = read.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	rows, err := read.QueryContext(ctx, `SELECT type,name,sql FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY type,name`)
	if err != nil {
		return err
	}
	ddl := []backupDDL{}
	for rows.Next() {
		var d backupDDL
		if err = rows.Scan(&d.kind, &d.name, &d.sql); err != nil {
			rows.Close()
			return err
		}
		ddl = append(ddl, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	dest, err := sql.Open("sqlite", DSN(path, false))
	if err != nil {
		return err
	}
	defer dest.Close()
	dest.SetMaxOpenConns(1)
	// Source DDL is the archive schema. Delay all triggers/indexes/views until rows
	// are sanitized. Temporary sorting never spills plaintext source values.
	if _, err = dest.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	if _, err = dest.ExecContext(ctx, "PRAGMA temp_store=MEMORY"); err != nil {
		return err
	}
	if _, err = dest.ExecContext(ctx, "PRAGMA journal_mode=DELETE"); err != nil {
		return err
	}
	var foreign int
	if err = dest.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreign); err != nil || foreign != 0 {
		return errors.New("backup_foreign_keys_enabled")
	}
	tx, err := dest.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tables := []string{}
	for _, d := range ddl {
		if d.kind == "table" {
			if _, err = tx.ExecContext(ctx, d.sql); err != nil {
				return err
			}
			tables = append(tables, d.name)
		}
	}
	content, norm := "content_objects", "normalizations"
	if version >= 8 {
		content, norm = "content_storage", "normalization_storage"
	}
	order := []string{content, norm}
	for _, name := range tables {
		if name != content && name != norm {
			order = append(order, name)
		}
	}
	revisions := map[string]string{}
	changed := false
	for _, table := range order {
		query := "SELECT t.* FROM " + quoteIdentifier(table) + " t"
		extras := 0
		if table == content {
			query += " JOIN content_objects c USING(digest)"
			query = "SELECT t.*,c.original_bytes FROM " + quoteIdentifier(table) + " t JOIN content_objects c USING(digest)"
			extras = 1
		}
		if table == norm {
			query = "SELECT t.*,n.envelope_json,c.original_bytes FROM " + quoteIdentifier(table) + " t JOIN normalizations n USING(record_key,source_revision,adapter_version) JOIN content_objects c ON c.digest=t.source_revision"
			extras = 2
		}
		rows, err = read.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		cols, e := rows.Columns()
		if e != nil {
			rows.Close()
			return e
		}
		cols = cols[:len(cols)-extras]
		positions := map[string]int{}
		names := make([]string, len(cols))
		marks := make([]string, len(cols))
		for i, name := range cols {
			positions[name] = i
			names[i] = quoteIdentifier(name)
			marks[i] = "?"
		}
		statement, e := tx.PrepareContext(ctx, "INSERT INTO "+quoteIdentifier(table)+"("+strings.Join(names, ",")+") VALUES("+strings.Join(marks, ",")+") ON CONFLICT DO NOTHING")
		if e != nil {
			rows.Close()
			return e
		}
		for rows.Next() {
			values := make([]any, len(cols)+extras)
			ptr := make([]any, len(values))
			for i := range values {
				ptr[i] = &values[i]
			}
			if err = rows.Scan(ptr...); err != nil {
				statement.Close()
				rows.Close()
				return err
			}
			set := func(name string, v any) {
				if i, ok := positions[name]; ok {
					values[i] = v
				}
			}
			if table == content {
				raw, ok := values[len(cols)].([]byte)
				if !ok || len(raw) > maxPayloadBytes {
					statement.Close()
					rows.Close()
					return errors.New("invalid_backup_payload")
				}
				counts := secretredact.Counts()
				if secretredact.Inspect(raw, counts) != nil {
					statement.Close()
					rows.Close()
					return secretredact.ErrSensitive
				}
				sanitized := secretredact.JSON(raw, nil)
				if !bytes.Equal(raw, sanitized) {
					changed = true
					if bytes.HasSuffix(raw, []byte("\n")) {
						sanitized = append(bytes.TrimRight(sanitized, "\r\n"), '\n')
					}
				}
				old, ok := values[positions["digest"]].(string)
				if !ok {
					statement.Close()
					rows.Close()
					return errors.New("invalid_backup_digest")
				}
				revision := sessionrecord.Digest(sanitized)
				revisions[old] = revision
				if len(revisions) > 2000000 {
					statement.Close()
					rows.Close()
					return errors.New("backup_record_limit")
				}
				set("digest", revision)
				set("original_bytes", sanitized)
				set("byte_length", int64(len(sanitized)))
				if version >= 8 {
					packed, n, e := storedPayload(sanitized)
					if e != nil {
						statement.Close()
						rows.Close()
						return e
					}
					set("original_bytes", packed)
					set("byte_length", int64(len(packed)))
					set("decoded_length", n)
				}
			} else if table == norm {
				envelope, ok := values[len(cols)].(string)
				if !ok {
					statement.Close()
					rows.Close()
					return errors.New("invalid_backup_envelope")
				}
				raw, ok := values[len(cols)+1].([]byte)
				if !ok || len(envelope) > maxPayloadBytes {
					statement.Close()
					rows.Close()
					return errors.New("invalid_backup_envelope")
				}
				rec, e := sessionrecord.Decode([]byte(envelope))
				if e != nil {
					statement.Close()
					rows.Close()
					return e
				}
				item, e := sessioncapture.Sanitize(sessioncapture.Captured{Record: rec, Raw: raw})
				if e != nil {
					statement.Close()
					rows.Close()
					return e
				}
				wire := encode(item.Record)
				if item.Redaction != nil {
					changed = true
				}
				set("source_revision", item.Record.SourceRevision)
				set("envelope_json", wire)
				if version >= 8 {
					packed, n, e := storedPayload([]byte(wire))
					if e != nil {
						statement.Close()
						rows.Close()
						return e
					}
					set("decoded_length", n)
					set("compressed_envelope", nil)
					set("envelope_digest", nil)
					if n >= 0 {
						set("envelope_json", "{}")
						set("compressed_envelope", packed)
						set("envelope_digest", sessionrecord.Digest([]byte(wire)))
					}
				}
			}
			{
				for i, v := range values[:len(cols)] {
					if s, ok := v.(string); ok {
						result, e := backupText(s, cols[i], revisions)
						if e != nil {
							statement.Close()
							rows.Close()
							return e
						}
						if result != s {
							changed = true
						}
						values[i] = result
					}
				}
			}
			if _, err = statement.ExecContext(ctx, values[:len(cols)]...); err != nil {
				statement.Close()
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		statement.Close()
		rows.Close()
		if err != nil {
			return err
		}
	}
	if changed {
		if _, err = tx.ExecContext(ctx, "UPDATE archive_meta SET archive_instance=?", NewID()); err != nil {
			return err
		}
		if version >= 6 {
			if _, err = tx.ExecContext(ctx, "UPDATE archive_scope SET generation=generation+1; DELETE FROM consumer_checkpoints"); err != nil {
				return err
			}
		}
	}
	for _, kind := range []string{"view", "index", "trigger"} {
		for _, d := range ddl {
			if d.kind == kind {
				if _, err = tx.ExecContext(ctx, d.sql); err != nil {
					return err
				}
			}
		}
	}

	if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return read.Commit()
}

func backupText(s, key string, revisions map[string]string) (string, error) {
	if key == "source_revision" {
		if next, ok := revisions[s]; ok {
			return next, nil
		}
	}
	counts := secretredact.Counts()
	if secretredact.Inspect([]byte(s), counts) != nil {
		return "", secretredact.ErrSensitive
	}
	out := secretredact.JSON([]byte(s), nil)
	// JSON references change only under an explicit source_revision key. Native
	// checkpoint prefix digests must keep describing the native file.
	var v any
	d := json.NewDecoder(bytes.NewReader(out))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return string(out), nil
	}
	changed := false
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				if k == "source_revision" {
					if old, ok := v.(string); ok {
						if next, ok := revisions[old]; ok && next != old {
							x[k] = next
							changed = true
						}
					}
				}
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(v)
	if changed {
		b, e := json.Marshal(v)
		if e != nil {
			return "", secretredact.ErrSensitive
		}
		return string(b), nil
	}
	return string(out), nil
}
