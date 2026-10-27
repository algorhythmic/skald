package archive

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"github.com/algorhythmic/skald/internal/secretredact"
	"github.com/algorhythmic/skald/internal/testsupport"
	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
	"os"
	"path/filepath"
	"testing"
)

// legacySecretStore deliberately bypasses the public sanitizer, to model an
// archive written by an older binary. All values are runtime synthetic fixtures.
func legacySecretStore(t *testing.T, input string) (*Store, Registration, sessioncapture.Captured) {
	t.Helper()
	s, r, _ := testStore(t)
	clean, _ := json.Marshal(map[string]any{"type": "user", "sessionId": "synthetic-claude", "uuid": "synthetic-record", "message": map[string]string{"role": "user", "content": "ordinary fixture"}})
	clean = append(clean, '\n')
	batch := testBatch(t, clean, r, sessioncapture.Checkpoint{})
	raw, _ := json.Marshal(map[string]any{"type": "user", "sessionId": "synthetic-claude", "uuid": "synthetic-record", "message": map[string]string{"role": "user", "content": input}})
	raw = append(raw, '\n')
	item := batch.Records[0]
	item.Raw = raw
	item.Record.Body.Text = input
	item.Record.SourceRevision = sessionrecord.Digest(raw)
	item.Record.RawRef = &sessionrecord.Ref{RecordKey: item.Record.RecordKey, SourceRevision: item.Record.SourceRevision}
	batch.Records[0] = item
	batch.Checkpoint.Offset = int64(len(raw))
	batch.Checkpoint.ParsedOffset = int64(len(raw))
	batch.Checkpoint.PrefixDigest = sessionrecord.Digest(raw)
	tx, e := s.db.BeginTx(context.Background(), nil)
	if e != nil {
		t.Fatal("legacy fixture transaction")
	}
	if _, e = s.ingestTx(context.Background(), tx, r, sessioncapture.Checkpoint{}, batch, testNow, nil); e != nil {
		tx.Rollback()
		t.Fatal("legacy fixture insertion")
	}
	if tx.Commit() != nil {
		t.Fatal("legacy fixture commit")
	}
	if _, e = s.db.Exec(`UPDATE session_titles SET title=?`, input); e != nil {
		t.Fatal("legacy title fixture")
	}
	return s, r, item
}

func checkSanitizedBackup(t *testing.T, path string, fixture testsupport.Secret) {
	t.Helper()
	if e := verifyFile(context.Background(), path, false); e != nil {
		t.Fatal("sanitized backup integrity")
	}
	db, e := sql.Open("sqlite", DSN(path, true)+"&immutable=1")
	if e != nil {
		t.Fatal("backup open")
	}
	defer db.Close()
	rows, e := db.Query(`SELECT original_bytes FROM content_objects`)
	if e != nil {
		t.Fatal("backup content query")
	}
	for rows.Next() {
		var b []byte
		if rows.Scan(&b) != nil || secretredact.Check(b) != nil {
			t.Fatal("backup content leaked")
		}
	}
	rows.Close()
	rows, e = db.Query(`SELECT envelope_json FROM normalizations`)
	if e != nil {
		t.Fatal("backup normalization query")
	}
	for rows.Next() {
		var wire string
		if rows.Scan(&wire) != nil || secretredact.Check([]byte(wire)) != nil {
			t.Fatal("backup envelope leaked")
		}
		rec, e := sessionrecord.Decode([]byte(wire))
		if e != nil || rec.Extensions["skald_redaction"] == nil {
			t.Fatal("sanitized backup representation missing")
		}
	}
	rows.Close()
	physical, e := os.ReadFile(path)
	if e != nil || bytes.Contains(physical, []byte(fixture.Value)) {
		t.Fatal("backup physical bytes leaked")
	}
}

func TestEveryPatternSanitizedBackupAndCompressedExport(t *testing.T) {
	requireZstd(t)
	for _, fixture := range testsupport.Secrets() {
		t.Run(fixture.Rule, func(t *testing.T) {
			s, _, item := legacySecretStore(t, fixture.Input)
			sourceInstance := s.instance
			backup, e := s.Backup(context.Background())
			if e != nil {
				t.Fatal("sanitized backup failed")
			}
			checkSanitizedBackup(t, backup, fixture)
			var original []byte
			if e = s.db.QueryRow(`SELECT original_bytes FROM content_objects WHERE digest=?`, item.Record.SourceRevision).Scan(&original); e != nil || !bytes.Equal(original, item.Raw) || s.instance != sourceInstance {
				t.Fatal("source archive was changed")
			}
			// Seal and copy ONLY this synthetic legacy fixture to exercise direct export
			// of a pre-redaction backup, independently of the already-sanitized Backup().
			source := filepath.Join(s.dir, "archive.sqlite")
			if s.Close() != nil {
				t.Fatal("fixture close")
			}
			raw, e := os.ReadFile(source)
			if e != nil {
				t.Fatal("fixture read")
			}
			legacy := filepath.Join(t.TempDir(), "legacy.sqlite")
			if os.WriteFile(legacy, raw, 0600) != nil {
				t.Fatal("fixture copy")
			}
			manifest, e := CompressBackup(context.Background(), legacy, "", DefaultMaxBackupBytes)
			if e != nil || manifest.RedactionVersion != 1 {
				t.Fatal("compressed sanitized export failed")
			}
			decoded := filepath.Join(t.TempDir(), "decoded.sqlite")
			if e = decompressBackup(context.Background(), legacy+".zst", decoded, manifest); e != nil {
				t.Fatal("sanitized decode failed")
			}
			checkSanitizedBackup(t, decoded, fixture)
			after, e := os.ReadFile(legacy)
			if e != nil || !bytes.Equal(raw, after) {
				t.Fatal("source backup was changed")
			}
		})
	}
}
