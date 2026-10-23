package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/algorhythmic/skald/internal/archive"
)

func TestBackupCompressAndRestoreCLI(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd executable unavailable")
	}
	s, err := archive.Open(filepath.Join(t.TempDir(), "data"), archive.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	backup, err := s.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := run([]string{"backup-compress", "--backup", backup}, &out, &stderr); err != nil {
		t.Fatal(err, stderr.String())
	}
	var manifest archive.BackupManifest
	if err := json.Unmarshal(out.Bytes(), &manifest); err != nil || manifest.SchemaVersion != archive.SchemaVersion {
		t.Fatal("compression result", manifest, err)
	}
	out.Reset()
	destination := filepath.Join(t.TempDir(), "restored")
	if err := run([]string{"restore", "--backup", backup + ".zst", "--data-dir", destination}, &out, &stderr); err != nil {
		t.Fatal(err, stderr.String())
	}
	var result map[string]string
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result["restored_to"] != destination {
		t.Fatal("restore result", result, err)
	}
}

func TestBackupCompressFlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"backup-compress"},
		{"backup-compress", "--backup", "missing", "--socket", "/tmp/skald.sock"},
		{"backup", "--max-backup-bytes", "1"},
		{"restore", "--backup", "missing", "--data-dir", "missing", "--destination", "invalid.zst"},
	} {
		var out, stderr bytes.Buffer
		if err := run(args, &out, &stderr); err == nil {
			t.Fatal("unsupported arguments accepted", args)
		}
	}
}
