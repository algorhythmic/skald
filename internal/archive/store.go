// Package archive owns Skald's independent SQLite archive. A process lock and
// serialized transactions give the daemon exclusive writer ownership.
package archive

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const SchemaVersion = 5
const MaxResponseBytes = 32 << 20

var ErrCapacity = errors.New("capture_capacity_limit")
var ErrCheckpoint = errors.New("checkpoint_conflict")

//go:embed migrations/*.sql
var migrations embed.FS

type Options struct {
	MaxDatabaseBytes int64
	MinFreeBytes     uint64
}

func DefaultOptions() Options { return Options{MaxDatabaseBytes: 4 << 30, MinFreeBytes: 256 << 20} }

type Store struct {
	mu       sync.Mutex
	db       *sql.DB
	lock     *os.File
	dir      string
	instance string
	options  Options
}

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || !ok || st.Uid != uint32(os.Getuid()) {
		return errors.New("private_owned_directory_required")
	}
	return nil
}
func regularPrivate(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || st.Uid != uint32(os.Getuid()) {
		return errors.New("private_owned_file_required")
	}
	return nil
}
func DSN(path string, readonly bool) string {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	if readonly {
		q.Set("mode", "ro")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func Open(dir string, options Options) (_ *Store, finalErr error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	if options.MaxDatabaseBytes < 1<<20 {
		return nil, errors.New("database_limit_too_small")
	}
	lockPath := filepath.Join(dir, "writer.lock")
	if err := regularPrivate(lockPath); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("archive_in_use")
	}
	s := &Store{lock: lock, dir: dir, options: options}
	defer func() {
		if finalErr != nil {
			s.Close()
		}
	}()
	path := filepath.Join(dir, "archive.sqlite")
	if err := regularPrivate(path); err != nil {
		return nil, err
	}
	s.db, err = sql.Open("sqlite", DSN(path, false))
	if err != nil {
		return nil, err
	}
	s.db.SetMaxOpenConns(1)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version > SchemaVersion {
		return nil, errors.New("unsupported_archive_schema")
	}
	if version == 0 {
		var tables int
		if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&tables); err != nil {
			return nil, err
		}
		if tables != 0 {
			return nil, errors.New("unrecognized_archive")
		}
		b, _ := migrations.ReadFile("migrations/001_capture.sql")
		if _, err := s.db.Exec(string(b)); err != nil {
			return nil, err
		}
		if _, err := s.db.Exec("INSERT INTO archive_meta VALUES (1,1,?,1)", NewID()); err != nil {
			return nil, err
		}
		version = 1
	} else if version == 1 {
		// Recover a process stop between the initial DDL commit and metadata insert.
		var metadata, sources int
		if err := s.db.QueryRow("SELECT count(*) FROM archive_meta").Scan(&metadata); err != nil {
			return nil, err
		}
		if metadata == 0 {
			if err := s.db.QueryRow("SELECT count(*) FROM sources").Scan(&sources); err != nil {
				return nil, err
			}
			if sources != 0 {
				return nil, errors.New("invalid_archive_metadata")
			}
			if _, err := s.db.Exec("INSERT INTO archive_meta VALUES (1,1,?,1)", NewID()); err != nil {
				return nil, err
			}
		}
		// Never mutate an older archive before a consistent recovery copy exists.
		if err := s.backupTo(context.Background(), filepath.Join(dir, "pre-schema-2-"+NewID()+".sqlite")); err != nil {
			return nil, err
		}
	}
	existing := version
	if version == 1 {
		b, _ := migrations.ReadFile("migrations/002_daemon.sql")
		if _, err := s.db.Exec(string(b)); err != nil {
			return nil, err
		}
		version = 2
	}
	if version == 2 {
		if existing == 2 {
			// Never mutate an older archive before a consistent recovery copy exists.
			if err := s.backupTo(context.Background(), filepath.Join(dir, "pre-schema-3-"+NewID()+".sqlite")); err != nil {
				return nil, err
			}
		}
		b, _ := migrations.ReadFile("migrations/003_titles.sql")
		if _, err := s.db.Exec(string(b)); err != nil {
			return nil, err
		}
		version = 3
	}
	if version == 3 {
		if existing == 3 {
			// Never mutate an older archive before a consistent recovery copy exists.
			if err := s.backupTo(context.Background(), filepath.Join(dir, "pre-schema-4-"+NewID()+".sqlite")); err != nil {
				return nil, err
			}
		}
		b, _ := migrations.ReadFile("migrations/004_activity.sql")
		if _, err := s.db.Exec(string(b)); err != nil {
			return nil, err
		}
		version = 4
	}
	if version == 4 {
		if existing == 4 {
			// Never mutate an older archive before a consistent recovery copy exists.
			if err := s.backupTo(context.Background(), filepath.Join(dir, "pre-schema-5-"+NewID()+".sqlite")); err != nil {
				return nil, err
			}
		}
		b, _ := migrations.ReadFile("migrations/005_continuation.sql")
		if _, err := s.db.Exec(string(b)); err != nil {
			return nil, err
		}
	}
	if err := s.db.QueryRow("SELECT archive_instance FROM archive_meta WHERE singleton=1 AND schema_version=5 AND contract_version=1").Scan(&s.instance); err != nil {
		return nil, errors.New("invalid_archive_metadata")
	}
	if err := s.reprojectTitles(context.Background()); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA wal_autocheckpoint=256"); err != nil {
		return nil, err
	}
	var size int64
	if err := s.db.QueryRow("PRAGMA page_size").Scan(&size); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA max_page_count=%d", options.MaxDatabaseBytes/size)); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.db != nil {
		err = s.db.Close()
		s.db = nil
	}
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
	return err
}
func encode(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func (s *Store) checkSpace(needed uint64) error {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(s.dir, &fs); err != nil {
		return err
	}
	available := fs.Bavail * uint64(fs.Bsize)
	if available < s.options.MinFreeBytes || available-s.options.MinFreeBytes < needed {
		return ErrCapacity
	}
	return nil
}
func (s *Store) boundary() (int64, error) {
	var n int64
	err := s.db.QueryRow("SELECT coalesce(max(sequence),0) FROM changes").Scan(&n)
	return n, err
}
