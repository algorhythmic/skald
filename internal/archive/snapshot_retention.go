package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type snapshotCompressor func(context.Context, string, string, int64) (BackupManifest, error)
type snapshotVerifier func(context.Context, string, string) error

func (s *Store) migrationBackupTo(ctx context.Context, path string) error {
	if err := s.backupTo(ctx, path); err != nil {
		return err
	}
	if err := retainMigrationSnapshot(ctx, path, compressExactSnapshot, verifySnapshotRoundTrip); err != nil {
		// Return a stable failure so Open stops before the next DDL. Raw stays put.
		return errors.New("migration_snapshot_retention_failed")
	}
	return nil
}
func compressExactSnapshot(ctx context.Context, source, destination string, limit int64) (BackupManifest, error) {
	return compressBackup(ctx, source, destination, limit, true)
}

func verifySnapshotRoundTrip(ctx context.Context, source, frame string) error {
	manifest, err := readBackupManifest(frame, DefaultMaxBackupBytes)
	if err != nil {
		return err
	}
	decoded, err := os.CreateTemp(filepath.Dir(frame), ".snapshot-roundtrip-*")
	if err != nil {
		return err
	}
	path := decoded.Name()
	if err = decoded.Close(); err != nil {
		os.Remove(path)
		return err
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	defer os.Remove(path)
	if err = decompressBackup(ctx, frame, path, manifest); err != nil {
		return err
	}
	if err = verifyBackupIdentity(ctx, path, manifest); err != nil {
		return err
	}
	// Decompression verified the decoded size/hash against the disk manifest.
	// Independently hash the raw snapshot and require the same exact bytes.
	raw, err := openBackupFile(source)
	if err != nil {
		return err
	}
	defer raw.Close()
	info, err := raw.Stat()
	if err != nil {
		return err
	}
	digest := sha256.New()
	n, err := io.Copy(digest, contextReader{ctx, raw})
	if err != nil {
		return err
	}
	if err = raw.Close(); err != nil {
		return err
	}
	if n != manifest.OriginalBytes || info.Size() != n || hex.EncodeToString(digest.Sum(nil)) != manifest.OriginalSHA256 {
		return errors.New("snapshot_roundtrip_mismatch")
	}
	return nil
}

func retainMigrationSnapshot(ctx context.Context, source string, compress snapshotCompressor, verify snapshotVerifier) (failure error) {
	before, err := os.Lstat(source)
	if err != nil {
		return err
	}
	parent := filepath.Dir(source)
	stage, err := os.MkdirTemp(parent, ".snapshot-retention-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	frame := filepath.Join(stage, filepath.Base(source)+".zst")
	if _, err = compress(ctx, source, frame, DefaultMaxBackupBytes); err != nil {
		return err
	}
	if err = verify(ctx, source, frame); err != nil {
		return err
	}
	destination := source + ".zst"
	for _, path := range []string{destination, destination + ".manifest.json"} {
		if _, err = os.Lstat(path); !os.IsNotExist(err) {
			return errors.New("snapshot_destination_exists")
		}
	}
	published := []string{}
	defer func() {
		if failure != nil {
			for _, path := range published {
				_ = os.Remove(path)
			}
		}
	}()
	// Staging is on the same filesystem; each rename atomically publishes one file.
	// A crash between files leaves the raw source intact, and restore refuses an
	// incomplete pair. No successful migration is reported until both are durable.
	if err = os.Rename(frame, destination); err != nil {
		return err
	}
	published = append(published, destination)
	if err = os.Rename(frame+".manifest.json", destination+".manifest.json"); err != nil {
		return err
	}
	published = append(published, destination+".manifest.json")
	if err = syncDirectory(parent); err != nil {
		return err
	}
	// Finish fallible staging cleanup before retiring raw recovery as well.
	if err = os.RemoveAll(stage); err != nil {
		return err
	}
	after, err := os.Lstat(source)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("snapshot_source_changed")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Last fallible operation: every reported failure above retains the raw file.
	// A crash before unlink persistence may retain an extra raw copy, safely.
	return os.Remove(source)
}
