package archive

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

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
	db, err := sql.Open("sqlite", DSN(path, true))
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
	return nil
}

// Restore only creates a fresh destination directory. It never opens or rewrites
// provider data or replaces an existing archive. New instance IDs expire cursors.
func Restore(ctx context.Context, backup, destination string, options Options) (finalErr error) {
	backup, err := filepath.Abs(backup)
	if err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	if err := verifyFile(ctx, backup, true); err != nil {
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
	source, err := sql.Open("sqlite", DSN(backup, true))
	if err != nil {
		return err
	}
	err = copyDatabase(ctx, source, path)
	source.Close()
	if err != nil {
		return err
	}
	if err := verifyFile(ctx, path, true); err != nil {
		return err
	}
	s, err := Open(destination, options)
	if err != nil {
		return err
	}
	defer s.Close()
	_, err = s.db.ExecContext(ctx, "UPDATE archive_meta SET restored_from=archive_instance,archive_instance=? WHERE singleton=1", NewID())
	return err
}
