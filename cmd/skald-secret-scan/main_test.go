package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyCountsMixedPayloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.sqlite")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal("fixture open")
	}
	for _, q := range []string{`CREATE TABLE content_storage(original_bytes BLOB,decoded_length INTEGER)`, `CREATE TABLE normalization_storage(envelope_json TEXT,compressed_envelope BLOB,decoded_length INTEGER)`, `CREATE TABLE session_titles(title TEXT)`, `CREATE TABLE session_projects(project TEXT)`} {
		if _, e = db.Exec(q); e != nil {
			t.Fatal("fixture schema")
		}
	}
	raw, _ := json.Marshal(map[string]string{"password": "synthetic"})
	var packed bytes.Buffer
	w := zlib.NewWriter(&packed)
	w.Write(raw)
	w.Close()
	if _, e = db.Exec(`INSERT INTO content_storage VALUES(?,NULL),(?,?)`, raw, packed.Bytes(), len(raw)); e != nil {
		t.Fatal("fixture insert")
	}
	db.Exec(`INSERT INTO normalization_storage VALUES(?,NULL,-1)`, string(raw))
	db.Close()
	before, _ := os.Stat(path)
	counts, e := scan(context.Background(), path)
	if e != nil {
		t.Fatal("scan failed")
	}
	if counts["password_assignment"] != 3 {
		t.Fatal("unexpected count")
	}
	after, _ := os.Stat(path)
	if before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		t.Fatal("archive changed")
	}
}
func TestMissingArchiveNotCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite")
	if _, e := scan(context.Background(), path); e == nil {
		t.Fatal("missing archive accepted")
	}
	if _, e := os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("archive created")
	}
}
