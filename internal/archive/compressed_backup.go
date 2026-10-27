package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const DefaultMaxBackupBytes int64 = 16 << 30

// BackupManifest describes exact rollback bytes, including their original schema.
// Compression never deletes or upgrades the source recovery file. The payload
// is a sanitized logical rebuild; OriginalSHA256 hashes that uncompressed payload,
// not a promise that it equals the source file bytes.
type BackupManifest struct {
	Version          int    `json:"version"`
	RedactionVersion int    `json:"redaction_version,omitempty"`
	Format           string `json:"format"`
	SourceName       string `json:"source_name"`
	SchemaVersion    int    `json:"schema_version"`
	ArchiveInstance  string `json:"archive_instance"`
	OriginalBytes    int64  `json:"original_bytes"`
	OriginalSHA256   string `json:"original_sha256"`
	CompressedBytes  int64  `json:"compressed_bytes"`
	CompressedSHA256 string `json:"compressed_sha256"`
}

// noSymlinks also rejects symlinked ancestors, so private staging stays rooted
// in the directory the operator selected.
func noSymlinks(path string) error {
	for {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("backup_symlink_refused")
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func openBackupFile(path string) (*os.File, error) {
	if err := noSymlinks(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("backup_regular_file_required")
	}
	return f, nil
}

func rejectLiveBackup(path string) error {
	if filepath.Base(path) == "archive.sqlite" {
		return errors.New("live_archive_compression_refused")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
			return errors.New("closed_backup_required")
		}
	}
	// An alias of a live archive is also forbidden, including hard links.
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := rejectBackupHardlinks(info); err != nil {
		return err
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if active, err := os.Stat(filepath.Join(dir, "archive.sqlite")); err == nil && os.SameFile(info, active) {
			return errors.New("live_archive_compression_refused")
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return nil
}

func rejectBackupHardlinks(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		// SQLite sidecar names belong to a pathname, so an alias in another
		// directory can hide the live database's WAL from an immutable read.
		return errors.New("backup_hardlink_refused")
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type boundedWriter struct {
	w    io.Writer
	left int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.left {
		return 0, errors.New("backup_size_limit")
	}
	n, err := w.w.Write(p)
	w.left -= int64(n)
	return n, err
}

func compressedLimit(raw int64) int64 { return raw + raw/256 + (1 << 20) }

func runZstd(ctx context.Context, decompress bool, src io.Reader, dst io.Writer) error {
	args := []string{"--quiet", "--no-progress", "--single-thread", "-3", "--stdout"}
	if decompress {
		args = []string{"--quiet", "--no-progress", "--decompress", "--memory=64MB", "--stdout"}
	}
	// Input/output are already-open private files, never shell command text.
	cmd := exec.CommandContext(ctx, "zstd", args...)
	cmd.Stdin = contextReader{ctx, src}
	cmd.Stdout = dst
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("backup_zstd_failed: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// CompressBackup streams a closed recovery SQLite file into a zstd frame. It
// independently decompresses and verifies exact bytes, SQLite integrity, and
// content digests before publishing a private no-replace file and manifest.
// It operates independently of Store and never holds the collector's scan lock.
func CompressBackup(ctx context.Context, source, destination string, maxBytes int64) (_ BackupManifest, finalErr error) {
	var manifest BackupManifest
	if maxBytes < 1 || maxBytes > DefaultMaxBackupBytes {
		return manifest, errors.New("invalid_backup_size_limit")
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return manifest, err
	}
	if destination == "" {
		destination = source + ".zst"
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return manifest, err
	}
	if !strings.HasSuffix(destination, ".zst") {
		return manifest, errors.New("zstd_destination_required")
	}
	if err := rejectLiveBackup(source); err != nil {
		return manifest, err
	}
	in, err := openBackupFile(source)
	if err != nil {
		return manifest, err
	}
	defer in.Close()
	before, err := in.Stat()
	if err != nil {
		return manifest, err
	}
	if err := rejectBackupHardlinks(before); err != nil {
		return manifest, err
	}
	if before.Size() < 1 || before.Size() > maxBytes {
		return manifest, errors.New("backup_size_limit")
	}
	parent := filepath.Dir(destination)
	if err := noSymlinks(parent); err != nil {
		return manifest, err
	}
	if err := privateDir(parent); err != nil {
		return manifest, err
	}
	for _, path := range []string{destination, destination + ".manifest.json"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return manifest, errors.New("backup_destination_exists")
		}
	}
	if err := verifyFile(ctx, source, false); err != nil {
		return manifest, err
	}
	stage, err := os.MkdirTemp(parent, ".backup-compress-*")
	if err != nil {
		return manifest, err
	}
	defer os.RemoveAll(stage)
	sanitizedPath := filepath.Join(stage, "sanitized.sqlite")
	sourceDB, err := sql.Open("sqlite", DSN(source, true)+"&immutable=1")
	if err != nil {
		return manifest, err
	}
	err = logicalBackup(ctx, sourceDB, sanitizedPath)
	sourceDB.Close()
	if err != nil {
		return manifest, err
	}
	if err = verifyFile(ctx, sanitizedPath, false); err != nil {
		return manifest, err
	}
	stream, err := openBackupFile(sanitizedPath)
	if err != nil {
		return manifest, err
	}
	defer stream.Close()
	streamInfo, err := stream.Stat()
	if err != nil || streamInfo.Size() > maxBytes {
		return manifest, errors.New("backup_size_limit")
	}
	out, err := os.OpenFile(filepath.Join(stage, "backup.zst"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return manifest, err
	}
	defer out.Close()
	rawHash, compressedHash := sha256.New(), sha256.New()
	input := io.TeeReader(io.LimitReader(stream, maxBytes+1), rawHash)
	output := &boundedWriter{w: io.MultiWriter(out, compressedHash), left: compressedLimit(streamInfo.Size())}
	if err := runZstd(ctx, false, input, output); err != nil {
		return manifest, err
	}
	if err := out.Sync(); err != nil {
		return manifest, err
	}
	stat, err := out.Stat()
	if err != nil {
		return manifest, err
	}
	after, err := in.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return manifest, errors.New("backup_source_changed")
	}
	pathInfo, err := os.Lstat(source)
	if err != nil || !os.SameFile(before, pathInfo) {
		return manifest, errors.New("backup_source_changed")
	}
	if err := noSymlinks(source); err != nil {
		return manifest, err
	}
	if err := rejectLiveBackup(source); err != nil {
		return manifest, err
	}
	manifest = BackupManifest{Version: 1, RedactionVersion: 1, Format: "zstd", SourceName: filepath.Base(source), OriginalBytes: streamInfo.Size(), OriginalSHA256: hex.EncodeToString(rawHash.Sum(nil)), CompressedBytes: stat.Size(), CompressedSHA256: hex.EncodeToString(compressedHash.Sum(nil))}
	rawPath := filepath.Join(stage, "verified.sqlite")
	if err := decompressBackup(ctx, out.Name(), rawPath, manifest); err != nil {
		return manifest, err
	}
	if err := verifyFile(ctx, rawPath, false); err != nil {
		return manifest, err
	}
	db, err := sql.Open("sqlite", DSN(rawPath, true)+"&immutable=1")
	if err != nil {
		return manifest, err
	}
	err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&manifest.SchemaVersion)
	if err == nil {
		err = db.QueryRowContext(ctx, "SELECT archive_instance FROM archive_meta WHERE singleton=1").Scan(&manifest.ArchiveInstance)
	}
	db.Close()
	if err != nil {
		return manifest, err
	}
	metadata, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return manifest, err
	}
	manifestPath := filepath.Join(stage, "manifest.json")
	if err := os.WriteFile(manifestPath, append(metadata, '\n'), 0600); err != nil {
		return manifest, err
	}
	f, err := os.OpenFile(manifestPath, os.O_RDWR, 0600)
	if err != nil {
		return manifest, err
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return manifest, err
	}
	if err := ctx.Err(); err != nil {
		return manifest, err
	}
	// Publish the frame first; an interruption before manifest publication leaves
	// a frame that restore refuses. The original recovery file always remains.
	if err := os.Link(out.Name(), destination); err != nil {
		return manifest, err
	}
	if err := os.Link(manifestPath, destination+".manifest.json"); err != nil {
		os.Remove(destination)
		return manifest, err
	}
	return manifest, syncDirectory(parent)
}

func readBackupManifest(path string, maxBytes int64) (BackupManifest, error) {
	var manifest BackupManifest
	f, err := openBackupFile(path + ".manifest.json")
	if err != nil {
		return manifest, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() > 16<<10 {
		return manifest, errors.New("invalid_backup_manifest")
	}
	decoder := json.NewDecoder(io.LimitReader(f, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return manifest, errors.New("invalid_backup_manifest")
	}
	if (manifest.RedactionVersion != 0 && manifest.RedactionVersion != 1) || manifest.Version != 1 || manifest.Format != "zstd" || manifest.OriginalBytes < 1 || manifest.OriginalBytes > maxBytes || manifest.CompressedBytes < 1 || manifest.CompressedBytes > compressedLimit(manifest.OriginalBytes) || manifest.SchemaVersion < 1 || manifest.SchemaVersion > SchemaVersion || len(manifest.ArchiveInstance) < 1 || len(manifest.ArchiveInstance) > 256 {
		return manifest, errors.New("invalid_backup_manifest")
	}
	for _, digest := range []string{manifest.OriginalSHA256, manifest.CompressedSHA256} {
		if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != sha256.Size {
			return manifest, errors.New("invalid_backup_manifest")
		}
	}
	return manifest, nil
}

func verifyBackupIdentity(ctx context.Context, path string, manifest BackupManifest) error {
	db, err := sql.Open("sqlite", DSN(path, true)+"&immutable=1")
	if err != nil {
		return err
	}
	defer db.Close()
	var version int
	var instance string
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, "SELECT archive_instance FROM archive_meta WHERE singleton=1").Scan(&instance); err != nil {
		return err
	}
	if version != manifest.SchemaVersion || instance != manifest.ArchiveInstance {
		return errors.New("backup_manifest_identity_mismatch")
	}
	return nil
}

func decompressBackup(ctx context.Context, source, destination string, manifest BackupManifest) error {
	in, err := openBackupFile(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || info.Size() != manifest.CompressedBytes {
		return errors.New("backup_compressed_size_mismatch")
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	rawHash, compressedHash := sha256.New(), sha256.New()
	output := &boundedWriter{w: io.MultiWriter(out, rawHash), left: manifest.OriginalBytes}
	if err := runZstd(ctx, true, io.TeeReader(io.LimitReader(in, manifest.CompressedBytes+1), compressedHash), output); err != nil {
		return err
	}
	if output.left != 0 || hex.EncodeToString(rawHash.Sum(nil)) != manifest.OriginalSHA256 || hex.EncodeToString(compressedHash.Sum(nil)) != manifest.CompressedSHA256 {
		return errors.New("backup_digest_mismatch")
	}
	return out.Sync()
}
