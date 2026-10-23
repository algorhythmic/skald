package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
)

func requireZstd(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd executable unavailable")
	}
}

func TestCompressedBackupExactBytesAndRestore(t *testing.T) {
	requireZstd(t)
	ctx := context.Background()
	s, registration, raw := testStore(t)
	batch := testBatch(t, raw, registration, sessioncapture.Checkpoint{})
	mustIngest(t, s, registration, sessioncapture.Checkpoint{}, batch)
	backup, err := s.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := CompressBackup(ctx, backup, "", DefaultMaxBackupBytes)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != SchemaVersion || manifest.ArchiveInstance != s.instance {
		t.Fatal("source identity lost", manifest)
	}
	compressed := backup + ".zst"
	for _, path := range []string{compressed, compressed + ".manifest.json"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("non-private output", path, info, err)
		}
	}
	decoded := filepath.Join(t.TempDir(), "decoded.sqlite")
	if err := decompressBackup(ctx, compressed, decoded, manifest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(decoded)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("decompression changed SQLite bytes", err)
	}
	got, err = os.ReadFile(backup)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("compression changed original", err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err := Restore(ctx, compressed, target, DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(target, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.instance == s.instance {
		t.Fatal("restored identity reused")
	}
	cp, err := restored.Checkpoint(ctx, registration.Key())
	if err != nil || cp != batch.Checkpoint {
		t.Fatal("checkpoint changed", err)
	}
	for _, item := range batch.Records {
		got, err := restored.Get(ctx, []string{registration.Namespace}, item.Record.RecordKey, item.Record.SourceRevision, item.Record.AdapterVersion, true)
		if err != nil || !bytes.Equal(got.Raw, item.Raw) {
			t.Fatal("original content identity changed", err)
		}
	}
	if err := Restore(ctx, compressed, target, DefaultOptions()); err == nil {
		t.Fatal("existing restore destination replaced")
	}
}

func TestCompressedBackupOldSchemaRestoresThroughMigration(t *testing.T) {
	requireZstd(t)
	dir := schemaSixFixture(t)
	backup := filepath.Join(dir, "pre-schema-7.sqlite")
	if err := os.Rename(filepath.Join(dir, "archive.sqlite"), backup); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := CompressBackup(context.Background(), backup, "", DefaultMaxBackupBytes)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 6 {
		t.Fatal("rollback schema upgraded", manifest.SchemaVersion)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err := Restore(context.Background(), backup+".zst", target, DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", DSN(filepath.Join(target, "archive.sqlite"), true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal("normal restore migration skipped", version, err)
	}
	got, err := os.ReadFile(backup)
	if err != nil || !bytes.Equal(original, got) {
		t.Fatal("rollback bytes modified", err)
	}
}

func TestCompressedBackupCorruptionAndBounds(t *testing.T) {
	requireZstd(t)
	s, _, _ := testStore(t)
	backup, err := s.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := CompressBackup(context.Background(), backup, "", DefaultMaxBackupBytes)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := os.ReadFile(backup + ".zst")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"truncated", "corrupted", "digest", "expanded-bound", "schema", "missing-manifest"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "backup.zst")
			content := append([]byte(nil), frame...)
			m := manifest
			switch name {
			case "truncated":
				content = content[:len(content)/2]
			case "corrupted":
				content[len(content)/2] ^= 0xff
			case "digest":
				m.OriginalSHA256 = string(bytes.Repeat([]byte("0"), 64))
			case "expanded-bound":
				m.OriginalBytes /= 2
			case "schema":
				m.SchemaVersion = 1
			}
			if err := os.WriteFile(path, content, 0600); err != nil {
				t.Fatal(err)
			}
			if name != "missing-manifest" {
				metadata, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+".manifest.json", metadata, 0600); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(dir, "restored")
			if err := Restore(context.Background(), path, target, DefaultOptions()); err == nil {
				t.Fatal("invalid compressed restore accepted")
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatal("invalid restore published destination", err)
			}
			staging, err := filepath.Glob(filepath.Join(dir, ".backup-restore-*"))
			if err != nil || len(staging) != 0 {
				t.Fatal("staging not cleaned", staging, err)
			}
		})
	}
	if _, err := CompressBackup(context.Background(), backup, filepath.Join(t.TempDir(), "limited.zst"), 1); err == nil {
		t.Fatal("compression size bound ignored")
	}
	if err := RestoreWithLimit(context.Background(), backup+".zst", filepath.Join(t.TempDir(), "restored"), DefaultOptions(), manifest.OriginalBytes-1); err == nil {
		t.Fatal("restore size bound ignored")
	}
	// A valid zstd frame with a forged lower expanded size must stop writing at
	// the bound rather than relying on a compressed checksum or frame header.
	var decoded bytes.Buffer
	m := manifest
	m.OriginalBytes = 1
	if err := runZstd(context.Background(), true, bytes.NewReader(frame), &boundedWriter{w: &decoded, left: m.OriginalBytes}); err == nil || decoded.Len() > 1 {
		t.Fatal("expanded write bound ignored", decoded.Len(), err)
	}
}

func TestCompressedBackupRefusesUnsafeFilesAndCancellation(t *testing.T) {
	requireZstd(t)
	s, _, _ := testStore(t)
	backup, err := s.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	destination := filepath.Join(dir, "backup.zst")
	if err := os.WriteFile(destination, []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CompressBackup(context.Background(), backup, destination, DefaultMaxBackupBytes); err == nil {
		t.Fatal("overwrote output")
	}
	if got, _ := os.ReadFile(destination); string(got) != "sentinel" {
		t.Fatal("destination changed")
	}
	link := filepath.Join(dir, "alias.sqlite")
	if err := os.Symlink(backup, link); err != nil {
		t.Fatal(err)
	}
	if _, err := CompressBackup(context.Background(), link, "", DefaultMaxBackupBytes); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := CompressBackup(context.Background(), filepath.Join(s.dir, "archive.sqlite"), "", DefaultMaxBackupBytes); err == nil {
		t.Fatal("active archive accepted")
	}
	hardlink := filepath.Join(s.dir, "alias.sqlite")
	if err := os.Link(filepath.Join(s.dir, "archive.sqlite"), hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := CompressBackup(context.Background(), hardlink, "", DefaultMaxBackupBytes); err == nil {
		t.Fatal("live hardlink accepted")
	}
	if err := os.WriteFile(backup+"-wal", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CompressBackup(context.Background(), backup, "", DefaultMaxBackupBytes); err == nil {
		t.Fatal("open WAL accepted")
	}
	if err := os.Remove(backup + "-wal"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CompressBackup(ctx, backup, "", DefaultMaxBackupBytes); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	if _, err := os.Stat(backup + ".zst"); !os.IsNotExist(err) {
		t.Fatal("cancelled output published", err)
	}
	if err := runZstd(ctx, false, bytes.NewReader([]byte("input")), &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatal("subprocess cancellation ignored", err)
	}
}

func TestCompressedBackupChecksCompressedHash(t *testing.T) {
	requireZstd(t)
	s, _, _ := testStore(t)
	backup, err := s.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := CompressBackup(context.Background(), backup, "", DefaultMaxBackupBytes)
	if err != nil {
		t.Fatal(err)
	}
	wrong := sha256.Sum256([]byte("wrong"))
	manifest.CompressedSHA256 = hex.EncodeToString(wrong[:])
	if err := decompressBackup(context.Background(), backup+".zst", filepath.Join(t.TempDir(), "out.sqlite"), manifest); err == nil {
		t.Fatal("compressed digest ignored")
	}
}

func TestBackupOperationsRejectSiblingLiveHardlink(t *testing.T) {
	s, _, _ := testStore(t)
	live := filepath.Join(s.dir, "archive.sqlite")
	if info, err := os.Stat(live + "-wal"); err != nil || info.Size() == 0 {
		t.Fatal("fixture requires uncheckpointed live WAL", info, err)
	}
	// A sibling directory gives the alias no archive.sqlite ancestor or local
	// sidecars. Checking only those paths would expose stale database pages.
	alias := filepath.Join(t.TempDir(), "recovery.sqlite")
	if err := os.Link(live, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(alias + "-wal"); !os.IsNotExist(err) {
		t.Fatal("alias unexpectedly has WAL", err)
	}
	if _, err := CompressBackup(context.Background(), alias, "", DefaultMaxBackupBytes); err == nil || err.Error() != "backup_hardlink_refused" {
		t.Fatal("sibling live hardlink accepted for compression", err)
	}
	for _, source := range []string{live, alias} {
		target := filepath.Join(t.TempDir(), "restored")
		if err := Restore(context.Background(), source, target, DefaultOptions()); err == nil {
			t.Fatal("live backup accepted for immutable restore", source)
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatal("rejected restore created destination", err)
		}
	}
}
