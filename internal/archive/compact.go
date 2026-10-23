package archive

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// CompactionPreview is filesystem/header metadata only. In WAL mode the header's
// freelist estimate can lag committed WAL records; apply rechecks SQLite itself.
type CompactionPreview struct {
	DataDir                string `json:"data_dir"`
	SchemaVersion          int    `json:"schema_version"`
	DatabaseBytes          int64  `json:"database_bytes"`
	DatabaseAllocatedBytes int64  `json:"database_allocated_bytes"`
	WALBytes               int64  `json:"wal_bytes"`
	WALAllocatedBytes      int64  `json:"wal_allocated_bytes"`
	PageSize               int64  `json:"page_size"`
	EstimatedReusableBytes int64  `json:"estimated_reusable_bytes"`
	AvailableBytes         uint64 `json:"available_bytes"`
	RequiredFreeBytes      uint64 `json:"required_free_bytes"`
	MaxDatabaseBytes       int64  `json:"max_database_bytes"`
	HeadroomAvailable      bool   `json:"headroom_available"`
	RequiresStoppedDaemon  bool   `json:"requires_stopped_daemon"`
}
type CompactionResult struct {
	Applied                 bool               `json:"applied"`
	Before                  CompactionPreview  `json:"before"`
	After                   *CompactionPreview `json:"after,omitempty"`
	ReclaimedDatabaseBytes  int64              `json:"reclaimed_database_bytes,omitempty"`
	ReclaimedAllocatedBytes int64              `json:"reclaimed_allocated_bytes,omitempty"`
}

func inspectCompactionFile(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || st.Uid != uint32(os.Getuid()) || st.Nlink!=1 {
		f.Close()
		return nil, nil, errors.New("private_owned_file_required")
	}
	return f, info, nil
}

// PreviewCompaction never opens SQLite, takes a writer lock, migrates, or creates
// files. It is suitable while the daemon runs; apply requires exclusive ownership.
func PreviewCompaction(dir string, options Options) (p CompactionPreview, err error) {
	if options.MaxDatabaseBytes < 1<<20 {
		return p, errors.New("database_limit_too_small")
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return p, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return p, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || !ok || st.Uid != uint32(os.Getuid()) {
		return p, errors.New("private_owned_directory_required")
	}
	f, info, err := inspectCompactionFile(filepath.Join(dir, "archive.sqlite"))
	if err != nil {
		return p, err
	}
	defer f.Close()
	var header [100]byte
	if _, err = io.ReadFull(f, header[:]); err != nil {
		return p, err
	}
	if !bytes.Equal(header[:16], []byte("SQLite format 3\x00")) {
		return p, errors.New("unrecognized_archive")
	}
	p = CompactionPreview{DataDir: dir, SchemaVersion: int(binary.BigEndian.Uint32(header[60:64])), DatabaseBytes: info.Size(), DatabaseAllocatedBytes: info.Sys().(*syscall.Stat_t).Blocks * 512, MaxDatabaseBytes: options.MaxDatabaseBytes, RequiresStoppedDaemon: true}
	p.PageSize = int64(binary.BigEndian.Uint16(header[16:18]))
	if p.PageSize == 1 {
		p.PageSize = 65536
	}
	if p.PageSize < 512 || p.PageSize > 65536 || p.PageSize&(p.PageSize-1) != 0 || p.DatabaseBytes%p.PageSize != 0 {
		return p, errors.New("invalid_archive_header")
	}
	p.EstimatedReusableBytes = int64(binary.BigEndian.Uint32(header[36:40])) * p.PageSize
	wal, walInfo, e := inspectCompactionFile(filepath.Join(dir, "archive.sqlite-wal"))
	if e == nil {
		wal.Close()
		p.WALBytes = walInfo.Size()
		p.WALAllocatedBytes = walInfo.Sys().(*syscall.Stat_t).Blocks * 512
	} else if !os.IsNotExist(e) {
		return p, e
	}
	var fs syscall.Statfs_t
	if err = syscall.Statfs(dir, &fs); err != nil {
		return p, err
	}
	p.AvailableBytes = fs.Bavail * uint64(fs.Bsize)
	reserve := uint64(p.DatabaseBytes) * 2
	p.RequiredFreeBytes = ^uint64(0)
	if options.MinFreeBytes <= ^uint64(0)-reserve {
		p.RequiredFreeBytes = reserve + options.MinFreeBytes
	}
	p.HeadroomAvailable = p.AvailableBytes >= options.MinFreeBytes && p.AvailableBytes-options.MinFreeBytes >= reserve && p.DatabaseBytes <= options.MaxDatabaseBytes
	return p, nil
}

// CompactOffline applies in-place SQLite VACUUM under the archive's exclusive
// writer lock. It keeps all recovery copies and preserves the archive instance.
func CompactOffline(ctx context.Context, dir string, options Options) (result CompactionResult, err error) {
	if err = ctx.Err(); err != nil {
		return result, err
	}
	result.Before, err = PreviewCompaction(dir, options)
	if err != nil {
		return result, err
	}
	if result.Before.SchemaVersion != SchemaVersion {
		return result, errors.New("archive_schema_upgrade_required")
	}
	if !result.Before.HeadroomAvailable {
		return result, ErrCapacity
	}
	s, err := Open(dir, options)
	if err != nil {
		return result, err
	}
	defer s.Close()
	return s.Compact(ctx)
}
func (s *Store) checkpointTruncate(ctx context.Context) error {
	var busy, log, checkpointed int
	if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("archive_checkpoint_busy")
	}
	return nil
}

// Compact reclaims SQLite freelist pages. It blocks all other Store operations;
// the offline CLI is its intended caller. VACUUM is a SQLite atomic transaction.
func (s *Store) Compact(ctx context.Context) (result CompactionResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = ctx.Err(); err != nil {
		return result, err
	}
	result.Before, err = PreviewCompaction(s.dir, s.options)
	if err != nil {
		return result, err
	}
	var pages, size int64
	if err = s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return result, err
	}
	if err = s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
		return result, err
	}
	actual := pages * size
	if actual < result.Before.DatabaseBytes {
		actual = result.Before.DatabaseBytes
	}
	if actual > s.options.MaxDatabaseBytes {
		return result, ErrCapacity
	}
	if err = s.checkSpace(uint64(actual) * 2); err != nil {
		return result, err
	}
	if err = s.checkpointTruncate(ctx); err != nil {
		return result, err
	}
	if err = verifyFile(ctx, filepath.Join(s.dir, "archive.sqlite"), true); err != nil {
		return result, err
	}
	// Recheck after checkpointing any WAL pages into the main file.
	result.Before, err = PreviewCompaction(s.dir, s.options)
	if err != nil {
		return result, err
	}
	if err = s.checkSpace(uint64(result.Before.DatabaseBytes) * 2); err != nil {
		return result, err
	}
	if _, err = s.db.ExecContext(ctx, "VACUUM"); err != nil {
		return result, capacityError(err)
	}
	result.Applied = true
	if err = s.checkpointTruncate(ctx); err != nil {
		return result, err
	}
	if err = s.setDatabaseLimit(); err != nil {
		return result, err
	}
	if err = verifyFile(ctx, filepath.Join(s.dir, "archive.sqlite"), true); err != nil {
		return result, err
	}
	after, err := PreviewCompaction(s.dir, s.options)
	if err != nil {
		return result, err
	}
	result.After = &after
	result.ReclaimedDatabaseBytes = result.Before.DatabaseBytes - after.DatabaseBytes
	result.ReclaimedAllocatedBytes = result.Before.DatabaseAllocatedBytes + result.Before.WALAllocatedBytes - after.DatabaseAllocatedBytes - after.WALAllocatedBytes
	return result, nil
}
