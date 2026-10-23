// Package maintenance implements opt-in recovery-file housekeeping. It never
// traverses provider directories and never selects the newest raw rollback.
package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
)

type Policy struct {
	KeepRaw  int           `json:"keep_raw"`
	MaxAge   time.Duration `json:"max_age"`
	MaxBytes int64         `json:"max_bytes"`
}
type Candidate struct {
	Path           string `json:"path"`
	SHA256         string `json:"sha256"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	identities     []os.FileInfo
}
type Plan struct {
	Directory       string      `json:"directory"`
	AllocatedBytes  int64       `json:"allocated_bytes"`
	Candidates      []Candidate `json:"candidates"`
	RemainingBytes  int64       `json:"remaining_bytes"`
	BudgetSatisfied bool        `json:"budget_satisfied"`
	Diagnostics     []string    `json:"diagnostics"`
	Verification    string      `json:"verification"`
}

func safe(path string, directory bool) (os.FileInfo, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("absolute_path_required")
	}
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		i, e := os.Lstat(p)
		if e != nil {
			return nil, e
		}
		if i.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("symlink_refused")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	i, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if directory {
		if !i.IsDir() || i.Mode().Perm()&0077 != 0 {
			return nil, errors.New("private_directory_required")
		}
	} else {
		if !i.Mode().IsRegular() {
			return nil, errors.New("regular_file_required")
		}
		if s, ok := i.Sys().(*syscall.Stat_t); !ok || s.Nlink != 1 {
			return nil, errors.New("hardlink_refused")
		}
	}
	return i, nil
}
func allocated(i os.FileInfo) int64 {
	if s, ok := i.Sys().(*syscall.Stat_t); ok {
		return s.Blocks * 512
	}
	return i.Size()
}
func digest(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), e
}

// Usage accounts actual allocated blocks, including WAL, spool, and recovery
// files below explicitly supplied roots. Symlinks and hardlinks are refused.
func Usage(roots []string) (map[string]int64, error) {
	result := map[string]int64{}
	for _, root := range roots {
		if _, e := safe(root, true); e != nil {
			return nil, e
		}
		var bytes int64
		e := filepath.Walk(root, func(p string, i os.FileInfo, e error) error {
			if e != nil {
				return e
			}
			if i.IsDir() {
				return nil
			}
			if _, e = safe(p, false); e != nil {
				return e
			}
			bytes += allocated(i)
			return nil
		})
		if e != nil {
			return nil, e
		}
		result[root] = bytes
	}
	return result, nil
}

// Verify performs a fresh restore in private scratch space. It checks both
// compressed digests, SQLite integrity, archive identity, and supported schema.
func readManifest(path string) (archive.BackupManifest, error) {
	var m archive.BackupManifest
	i, e := safe(path, false)
	if e != nil {
		return m, e
	}
	if i.Size() > 16384 {
		return m, errors.New("manifest_too_large")
	}
	f, e := os.Open(path)
	if e != nil {
		return m, e
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, 16385))
	if e != nil {
		return m, e
	}
	if len(data) > 16384 {
		return m, errors.New("manifest_too_large")
	}
	e = json.Unmarshal(data, &m)
	return m, e
}
func Verify(ctx context.Context, backup string) error {
	info, e := safe(backup, false)
	if e != nil {
		return e
	}
	expanded := info.Size()
	if strings.HasSuffix(backup, ".zst") {
		m, e := readManifest(backup + ".manifest.json")
		if e != nil {
			return e
		}
		expanded = m.OriginalBytes
	}
	if expanded < 1 || expanded > archive.DefaultMaxBackupBytes {
		return errors.New("invalid_backup_size")
	}
	// Restore has staging, a fresh database and schema migration headroom.
	if e = RequireSpace(os.TempDir(), uint64(expanded)*3+(256<<20)); e != nil {
		return e
	}
	scratch, e := os.MkdirTemp("", "skald-recovery-check-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(scratch)
	options := archive.DefaultOptions()
	options.MaxDatabaseBytes = archive.DefaultMaxBackupBytes
	return archive.RestoreWithLimit(ctx, backup, filepath.Join(scratch, "restored"), options, archive.DefaultMaxBackupBytes)
}
func directoryLock(ctx context.Context, dir string, exclusive bool) (*os.File, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	info, e := safe(dir, true)
	if e != nil {
		return nil, e
	}
	f, e := os.Open(dir)
	if e != nil {
		return nil, e
	}
	current, e := f.Stat()
	if e != nil || !os.SameFile(info, current) {
		f.Close()
		return nil, errors.New("recovery_directory_changed")
	}
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	for {
		e = syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
		if e == nil {
			return f, nil
		}
		if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
			f.Close()
			return nil, e
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }
func PlanRetention(ctx context.Context, dir string, p Policy, now time.Time) (Plan, error) {
	lock, e := directoryLock(ctx, dir, false)
	if e != nil {
		return Plan{}, e
	}
	defer unlock(lock)
	return planRetention(ctx, dir, p, now)
}
func planRetention(ctx context.Context, dir string, p Policy, now time.Time) (Plan, error) {
	plan := Plan{Directory: dir, Candidates: []Candidate{}, Diagnostics: []string{}, Verification: "preview hashes only; apply performs fresh restore"}
	if p.KeepRaw < 1 || p.MaxAge < 0 || p.MaxBytes < 0 {
		return plan, errors.New("invalid_retention_policy")
	}
	if _, e := safe(dir, true); e != nil {
		return plan, e
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		return plan, e
	}
	type file struct {
		path string
		info os.FileInfo
	}
	var raws []file
	// Account every allocated file block, including unrelated live/WAL/spool
	// files, without following links or ever making those entries candidates.
	e = filepath.Walk(dir, func(_ string, i os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if !i.IsDir() {
			plan.AllocatedBytes += allocated(i)
		}
		return nil
	})
	if e != nil {
		return plan, e
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !(strings.HasPrefix(name, "archive-") || strings.HasPrefix(name, "pre-schema-")) || !strings.HasSuffix(name, ".sqlite") {
			continue
		}
		path := filepath.Join(dir, name)
		i, e := safe(path, false)
		if e != nil {
			return plan, e
		}
		raws = append(raws, file{path, i})
	}
	sort.Slice(raws, func(i, j int) bool {
		if raws[i].info.ModTime().Equal(raws[j].info.ModTime()) {
			return raws[i].path > raws[j].path
		}
		return raws[i].info.ModTime().After(raws[j].info.ModTime())
	})
	plan.RemainingBytes = plan.AllocatedBytes
	for index, f := range raws {
		if index < p.KeepRaw {
			continue
		}
		ageDue := p.MaxAge > 0 && now.Sub(f.info.ModTime()) > p.MaxAge
		countDue := p.MaxAge == 0
		bytesDue := p.MaxBytes > 0 && plan.RemainingBytes > p.MaxBytes
		if !ageDue && !countDue && !bytesDue {
			continue
		}
		if e := ctx.Err(); e != nil {
			return plan, e
		}
		compressed := f.path + ".zst"
		compressedInfo, e := safe(compressed, false)
		if os.IsNotExist(e) {
			plan.Diagnostics = append(plan.Diagnostics, filepath.Base(f.path)+": retained; no compressed sibling")
			continue
		}
		if e != nil {
			return plan, e
		}
		manifestInfo, e := safe(compressed+".manifest.json", false)
		if os.IsNotExist(e) {
			plan.Diagnostics = append(plan.Diagnostics, filepath.Base(f.path)+": retained; no manifest")
			continue
		}
		if e != nil {
			return plan, e
		}
		if e = sealed(f.path); e != nil {
			return plan, e
		}
		manifest, e := readManifest(compressed + ".manifest.json")
		if e != nil {
			return plan, e
		}
		hash, e := digest(f.path)
		if e != nil {
			return plan, e
		}
		packedHash, e := digest(compressed)
		if e != nil {
			return plan, e
		}
		if manifest.Version != 1 || manifest.Format != "zstd" || manifest.SourceName != filepath.Base(f.path) || manifest.OriginalBytes != f.info.Size() || manifest.OriginalSHA256 != hash || manifest.CompressedSHA256 != packedHash || manifest.CompressedBytes != compressedInfo.Size() {
			return plan, errors.New("raw_manifest_mismatch")
		}
		identities := []os.FileInfo{f.info, compressedInfo, manifestInfo}
		if e = unchanged(f.path, identities); e != nil {
			return plan, e
		}
		plan.Candidates = append(plan.Candidates, Candidate{Path: f.path, SHA256: hash, AllocatedBytes: allocated(f.info), identities: identities})
		plan.RemainingBytes -= allocated(f.info)
	}
	plan.BudgetSatisfied = p.MaxBytes == 0 || plan.RemainingBytes <= p.MaxBytes
	return plan, nil
}

// ApplyRetention recalculates all validation immediately before removal. Callers
// must require explicit operator apply; preview is always the CLI default.
func sealed(path string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, e := os.Lstat(path + suffix); e == nil {
			return errors.New("sealed_recovery_required")
		} else if !os.IsNotExist(e) {
			return e
		}
	}
	return nil
}
func unchanged(path string, identities []os.FileInfo) error {
	for index, suffix := range []string{"", ".zst", ".zst.manifest.json"} {
		i, e := safe(path+suffix, false)
		if e != nil {
			return e
		}
		before := identities[index]
		if !os.SameFile(before, i) || before.Size() != i.Size() || !before.ModTime().Equal(i.ModTime()) {
			return errors.New("recovery_changed")
		}
		a := before.Sys().(*syscall.Stat_t)
		b := i.Sys().(*syscall.Stat_t)
		if a.Ctim != b.Ctim {
			return errors.New("recovery_changed")
		}
	}
	return sealed(path)
}
func ApplyRetention(ctx context.Context, dir string, p Policy, now time.Time) (Plan, error) {
	lock, e := directoryLock(ctx, dir, true)
	if e != nil {
		return Plan{}, e
	}
	defer unlock(lock)
	dirInfo, e := lock.Stat()
	if e != nil {
		return Plan{}, e
	}
	plan, e := planRetention(ctx, dir, p, now)
	if e != nil {
		return plan, e
	}
	for _, c := range plan.Candidates {
		if e = ctx.Err(); e != nil {
			return plan, e
		}
		if e = unchanged(c.Path, c.identities); e != nil {
			return plan, e
		}
		if e = Verify(ctx, c.Path+".zst"); e != nil {
			return plan, e
		}
		hash, e := digest(c.Path)
		if e != nil {
			return plan, e
		}
		if hash != c.SHA256 {
			return plan, errors.New("recovery_changed")
		}
		if e = unchanged(c.Path, c.identities); e != nil {
			return plan, e
		}
		current, e := safe(dir, true)
		if e != nil {
			return plan, e
		}
		if !os.SameFile(dirInfo, current) {
			return plan, errors.New("recovery_directory_changed")
		}
		if e = syscall.Unlinkat(int(lock.Fd()), filepath.Base(c.Path)); e != nil {
			return plan, e
		}
		if e = lock.Sync(); e != nil {
			return plan, e
		}
	}
	plan.Verification = "applied after exact sibling hashes and fresh compressed restore"
	return plan, nil
}

// ExportBackup copies through compression into an independent operator-selected
// private directory and validates fresh restore. It never removes the source.
func ExportBackup(ctx context.Context, source, destinationDir string) (archive.BackupManifest, error) {
	return ExportBackupConfigured(ctx, source, BackupConfig{Destination: destinationDir})
}
func exportBackup(ctx context.Context, source, destinationDir string) (archive.BackupManifest, error) {
	var empty archive.BackupManifest
	if _, e := safe(destinationDir, true); e != nil {
		return empty, e
	}
	sourceDir := filepath.Clean(filepath.Dir(source))
	destinationDir = filepath.Clean(destinationDir)
	overlaps := func(a, b string) bool {
		relative, e := filepath.Rel(a, b)
		return e == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
	}
	if overlaps(sourceDir, destinationDir) || overlaps(destinationDir, sourceDir) {
		return empty, errors.New("independent_destination_required")
	}
	info, e := safe(source, false)
	if e != nil {
		return empty, e
	}
	if e = RequireSpace(destinationDir, uint64(info.Size())*2); e != nil {
		return empty, e
	}
	dest := filepath.Join(destinationDir, filepath.Base(source)+".zst")
	manifest, e := archive.CompressBackup(ctx, source, dest, archive.DefaultMaxBackupBytes)
	if e != nil {
		return empty, e
	}
	if e = Verify(ctx, dest); e != nil {
		return empty, e
	}
	return manifest, nil
}

// RequireSpace reserves headroom before a backup job; allocation errors during
// writing are still surfaced without removing source recovery files.
func RequireSpace(dir string, needed uint64) error {
	var fs syscall.Statfs_t
	if e := syscall.Statfs(dir, &fs); e != nil {
		return e
	}
	if needed > fs.Bavail*uint64(fs.Bsize) {
		return errors.New("insufficient_recovery_space")
	}
	return nil
}
