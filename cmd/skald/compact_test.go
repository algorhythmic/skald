package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/algorhythmic/skald/internal/archive"
)

func TestArchiveCompactDefaultsToReadOnlyPreview(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "archive")
	s, err := archive.Open(dir, archive.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	path := filepath.Join(dir, "archive.sqlite")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err = runArchiveCompact([]string{"--data-dir", dir}, &out, &stderr); err != nil {
		t.Fatal(err, stderr.String())
	}
	var preview archive.CompactionResult
	if err = json.Unmarshal(out.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Applied || preview.After != nil || preview.Before.DatabaseBytes == 0 || !preview.Before.RequiresStoppedDaemon {
		t.Fatal(preview)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("default compact modified archive", err)
	}
	out.Reset()
	stderr.Reset()
	if err = runArchiveCompact([]string{"--data-dir", dir, "--apply"}, &out, &stderr); err != nil {
		t.Fatal(err, stderr.String())
	}
	var applied archive.CompactionResult
	if err = json.Unmarshal(out.Bytes(), &applied); err != nil || !applied.Applied || applied.After == nil {
		t.Fatal(applied, err)
	}
}
