package archive

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func migrationSnapshots(t *testing.T, dir, pattern string) ([]string, error) {
	t.Helper()
	frames, err := filepath.Glob(filepath.Join(dir, pattern+".zst"))
	if err != nil {
		return nil, err
	}
	raw, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 {
		t.Fatal("successful migration retained uncompressed snapshots")
	}
	decoded := []string{}
	for _, frame := range frames {
		manifest, err := readBackupManifest(frame, DefaultMaxBackupBytes)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(t.TempDir(), "snapshot.sqlite")
		if err = decompressBackup(context.Background(), frame, path, manifest); err != nil {
			return nil, err
		}
		decoded = append(decoded, path)
	}
	return decoded, nil
}
func syntheticSnapshot(t *testing.T) string {
	t.Helper()
	requireZstd(t)
	s, _, _ := testStore(t)
	if _, err := s.db.Exec(`CREATE TABLE synthetic_padding(payload TEXT)`); err != nil {
		t.Fatal("fixture table")
	}
	if _, err := s.db.Exec(`INSERT INTO synthetic_padding VALUES (?)`, strings.Repeat("ordinary-fixture-padding-", 32768)); err != nil {
		t.Fatal("fixture padding")
	}
	path, err := s.Backup(context.Background())
	if err != nil {
		t.Fatal("fixture snapshot")
	}
	return path
}
func TestMigrationSnapshotRetentionRoundTrip(t *testing.T) {
	source := syntheticSnapshot(t)
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal("fixture bytes")
	}
	if err = retainMigrationSnapshot(context.Background(), source, compressExactSnapshot, verifySnapshotRoundTrip); err != nil {
		t.Fatal("retention failed", err)
	}
	if _, err = os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("raw snapshot retained after success")
	}
	manifest, err := readBackupManifest(source+".zst", DefaultMaxBackupBytes)
	if err != nil {
		t.Fatal("published manifest")
	}
	decoded := filepath.Join(t.TempDir(), "decoded.sqlite")
	if err = decompressBackup(context.Background(), source+".zst", decoded, manifest); err != nil {
		t.Fatal("published roundtrip")
	}
	recovered, err := os.ReadFile(decoded)
	if err != nil || !bytes.Equal(raw, recovered) {
		t.Fatal("snapshot not byte exact")
	}
	t.Logf("logical_raw_bytes=%d logical_compressed_bytes=%d logical_ratio=%.2f", len(raw), manifest.CompressedBytes, float64(len(raw))/float64(manifest.CompressedBytes))
}
func TestMigrationSnapshotRetentionKeepsRawOnFailure(t *testing.T) {
	for _, name := range []string{"verify_mismatch", "partial_write", "interrupted", "manifest_publication"} {
		t.Run(name, func(t *testing.T) {
			source := syntheticSnapshot(t)
			raw, err := os.ReadFile(source)
			if err != nil {
				t.Fatal("fixture bytes")
			}
			compress := snapshotCompressor(compressExactSnapshot)
			if name == "partial_write" {
				compress = func(ctx context.Context, s, d string, limit int64) (BackupManifest, error) {
					if err := os.WriteFile(d, []byte("synthetic-partial-frame"), 0600); err != nil {
						return BackupManifest{}, err
					}
					return BackupManifest{}, io.ErrUnexpectedEOF
				}
			}
			if name == "verify_mismatch" {
				compress = func(ctx context.Context, s, d string, limit int64) (BackupManifest, error) {
					m, e := compressExactSnapshot(ctx, s, d, limit)
					if e != nil {
						return m, e
					}
					m.OriginalSHA256 = strings.Repeat("0", 64)
					wire, _ := json.Marshal(m)
					return m, os.WriteFile(d+".manifest.json", wire, 0600)
				}
			}
			verify := snapshotVerifier(verifySnapshotRoundTrip)
			if name == "manifest_publication" {
				verify = func(ctx context.Context, source, frame string) error {
					if err := verifySnapshotRoundTrip(ctx, source, frame); err != nil {
						return err
					}
					return os.Remove(frame + ".manifest.json")
				}
			}
			ctx := context.Background()
			if name == "interrupted" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err = retainMigrationSnapshot(ctx, source, compress, verify); err == nil {
				t.Fatal("failure not reported")
			}
			remaining, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(raw, remaining) {
				t.Fatal("only raw copy lost or changed")
			}
			for _, pattern := range []string{".snapshot-retention-*", ".backup-compress-*", "*.zst", "*.manifest.json"} {
				files, _ := filepath.Glob(filepath.Join(filepath.Dir(source), pattern))
				if len(files) != 0 {
					t.Fatal("partial output left behind")
				}
			}
		})
	}
}

func TestMigrationRetentionFailureStopsDDL(t *testing.T) {
	// No compressor means no destructive migration, with its raw recovery retained.
	dir := schemaSixFixture(t)
	t.Setenv("PATH", t.TempDir())
	s, err := Open(dir, DefaultOptions())
	if s != nil {
		s.Close()
	}
	if err == nil || err.Error() != "migration_snapshot_retention_failed" {
		t.Fatal("missing compressor did not stop migration")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "pre-schema-7-*.sqlite"))
	if len(files) != 1 {
		t.Fatal("failed migration lost raw recovery")
	}
	db, err := sql.Open("sqlite", DSN(filepath.Join(dir, "archive.sqlite"), true))
	if err != nil {
		t.Fatal("fixture open")
	}
	defer db.Close()
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 6 {
		t.Fatal("failed retention advanced schema")
	}
}
