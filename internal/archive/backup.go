package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/algorhythmic/skald/sessionrecord"
	"modernc.org/sqlite"
)

type backuper interface {
	NewBackup(string) (*sqlite.Backup, error)
}

func copyDatabase(ctx context.Context, db *sql.DB, destination string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(driver any) (err error) {
		b, err := driver.(backuper).NewBackup(DSN(destination, false))
		if err != nil {
			return err
		}
		defer func() {
			if e := b.Finish(); err == nil {
				err = e
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := b.Step(128)
			if err != nil {
				return err
			}
			if !more {
				return nil
			}
		}
	})
}

// backupTo publishes only after SQLite's online backup API and integrity checks
// finish. Temporary files have private permissions and are never user-visible IDs.
func (s *Store) backupTo(ctx context.Context, path string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".backup-*.sqlite")
	if err != nil {
		return err
	}
	temp := f.Name()
	if err := f.Close(); err != nil {
		return err
	}
	defer os.Remove(temp)
	if err := copyDatabase(ctx, s.db, temp); err != nil {
		return err
	}
	if err := verifyFile(ctx, temp, false); err != nil {
		return err
	}
	f, err = os.OpenFile(temp, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return err
	}
	// Link is an atomic no-replace publication, unlike Rename on Unix.
	if err := os.Link(temp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Store) Backup(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pages, size int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return "", err
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
		return "", err
	}
	if err := s.checkSpace(uint64(pages * size)); err != nil {
		return "", err
	}
	dir := filepath.Join(s.dir, "backups")
	if err := privateDir(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "archive-"+NewID()+".sqlite")
	if err := s.backupTo(ctx, path); err != nil {
		return "", err
	}
	return path, nil
}

func verifyFile(ctx context.Context, path string, current bool) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("backup_unavailable")
	}
	// Immutable reads avoid creating SQLite sidecars for sealed recovery files.
	// Reject uncheckpointed data rather than silently ignoring an existing WAL.
	for _, suffix := range []string{"-wal", "-journal"} {
		if sidecar, err := os.Lstat(path + suffix); err == nil {
			if !sidecar.Mode().IsRegular() || sidecar.Size() != 0 {
				return errors.New("closed_backup_required")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	db, err := sql.Open("sqlite", DSN(path, true)+"&immutable=1")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 1 || version > SchemaVersion || (current && version != SchemaVersion) {
		return errors.New("unsupported_archive_schema")
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return errors.New("backup_integrity_failed")
	}
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	bad := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if bad {
		return errors.New("backup_foreign_keys_failed")
	}
	var oversized int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM content_objects WHERE byte_length>67108864 OR byte_length!=length(original_bytes)").Scan(&oversized); err != nil {
		return err
	}
	if oversized != 0 {
		return errors.New("backup_object_limit")
	}
	rows, err = db.QueryContext(ctx, "SELECT digest,original_bytes FROM content_objects")
	if err != nil {
		return err
	}
	for rows.Next() {
		var digest string
		var raw []byte
		if err := rows.Scan(&digest, &raw); err != nil {
			rows.Close()
			return err
		}
		if sessionrecord.Digest(raw) != digest {
			rows.Close()
			return errors.New("backup_digest_mismatch")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if version >= 8 {
		rows, err = db.QueryContext(ctx, "SELECT envelope_json FROM normalizations")
		if err != nil {
			return err
		}
		for rows.Next() {
			var envelope string
			if err = rows.Scan(&envelope); err != nil {
				rows.Close()
				return err
			}
			if len(envelope) > maxPayloadBytes || !json.Valid([]byte(envelope)) {
				rows.Close()
				return errors.New("backup_normalization_invalid")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// Restore only creates a fresh destination directory. It never opens or rewrites
// provider data or replaces an existing archive. New instance IDs expire cursors.
func Restore(ctx context.Context, backup, destination string, options Options) (finalErr error) {
	return RestoreWithLimit(ctx, backup, destination, options, DefaultMaxBackupBytes)
}

// RestoreWithLimit bounds the expanded bytes of a compressed recovery file.
func RestoreWithLimit(ctx context.Context, backup, destination string, options Options, maxBytes int64) (finalErr error) {
	if maxBytes < 1 || maxBytes > DefaultMaxBackupBytes {
		return errors.New("invalid_backup_size_limit")
	}
	backup, err := filepath.Abs(backup)
	if err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	if err := noSymlinks(backup); err != nil {
		return err
	}
	if err := noSymlinks(filepath.Dir(destination)); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return errors.New("fresh_restore_directory_required")
	}
	if strings.HasSuffix(backup, ".zst") {
		manifest, err := readBackupManifest(backup, maxBytes)
		if err != nil {
			return err
		}
		stage, err := os.MkdirTemp(filepath.Dir(destination), ".backup-restore-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(stage)
		staged := filepath.Join(stage, "recovery.sqlite")
		if err := decompressBackup(ctx, backup, staged, manifest); err != nil {
			return err
		}
		if err := verifyBackupIdentity(ctx, staged, manifest); err != nil {
			return err
		}
		backup = staged
	} else {
		// Immutable SQLite reads require a sealed recovery file. In particular,
		// a hard link can conceal a live archive's WAL in another directory.
		if err := rejectLiveBackup(backup); err != nil {
			return err
		}
		if info, err := os.Stat(backup); err != nil || info.Size() > maxBytes {
			return errors.New("backup_size_limit")
		}
	}
	if err := verifyFile(ctx, backup, false); err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return errors.New("fresh_restore_directory_required")
	}
	// On failure retain the private directory for diagnosis; never overwrite it.
	path := filepath.Join(destination, "archive.sqlite")
	if err := regularPrivate(path); err != nil {
		return err
	}
	source, err := sql.Open("sqlite", DSN(backup, true)+"&immutable=1")
	if err != nil {
		return err
	}
	err = copyDatabase(ctx, source, path)
	source.Close()
	if err != nil {
		return err
	}
	if err := verifyFile(ctx, path, false); err != nil {
		return err
	}
	// Open upgrades older backup schemas and keeps a consistent recovery copy.
	s, err := Open(destination, options)
	if err != nil {
		return err
	}
	defer s.Close()
	_, err = s.db.ExecContext(ctx, "UPDATE archive_meta SET restored_from=archive_instance,archive_instance=? WHERE singleton=1", NewID())
	return err
}
