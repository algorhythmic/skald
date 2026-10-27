package maintenance

import (
	"context"
	"github.com/algorhythmic/skald/internal/archive"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPreviewApplyAndBudget(t *testing.T) {
	ctx := context.Background()
	store, e := archive.Open(filepath.Join(t.TempDir(), "live"), archive.DefaultOptions())
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	first, e := store.Backup(ctx)
	if e != nil {
		t.Fatal(e)
	}
	old := time.Now().Add(-time.Hour)
	if e = os.Chtimes(first, old, old); e != nil {
		t.Fatal(e)
	}
	if _, e = archive.CompressBackup(ctx, first, "", archive.DefaultMaxBackupBytes); e != nil {
		t.Fatal(e)
	}
	second, e := store.Backup(ctx)
	if e != nil {
		t.Fatal(e)
	}
	dir := filepath.Dir(first)
	p := Policy{KeepRaw: 1, MaxBytes: 1}
	plan, e := PlanRetention(ctx, dir, p, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Path != first || plan.BudgetSatisfied {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if _, e = os.Stat(first); e != nil {
		t.Fatal("preview deleted raw")
	}
	if _, e = ApplyRetention(ctx, dir, p, time.Now()); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(first); !os.IsNotExist(e) {
		t.Fatal("raw not retired")
	}
	if _, e = os.Stat(second); e != nil {
		t.Fatal("newest raw removed")
	}
	if e = Verify(ctx, first+".zst"); e != nil {
		t.Fatal(e)
	}
}
func TestUnsafeAndUnverifiedRefused(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	for _, name := range []string{"archive-a.sqlite", "archive-b.sqlite"} {
		if e := os.WriteFile(filepath.Join(dir, name), []byte("unverified"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	plan, e := PlanRetention(context.Background(), dir, Policy{KeepRaw: 1}, time.Now())
	if e != nil || len(plan.Candidates) != 0 || len(plan.Diagnostics) != 1 {
		t.Fatalf("unpaired raw must remain: %+v %v", plan, e)
	}
	if e = os.WriteFile(filepath.Join(dir, "archive-a.sqlite.zst"), []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "archive-a.sqlite.zst.manifest.json"), []byte("{}"), 0600); e != nil {
		t.Fatal(e)
	}
	// Ensure the corrupt paired file is older and eligible.
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "archive-a.sqlite"), old, old)
	if _, e = PlanRetention(context.Background(), dir, Policy{KeepRaw: 1}, time.Now()); e == nil {
		t.Fatal("corrupt pair accepted")
	}
	if e := os.Symlink(filepath.Join(dir, "archive-a.sqlite"), filepath.Join(dir, "link")); e != nil {
		t.Fatal(e)
	}
	if _, e := Usage([]string{dir}); e == nil {
		t.Fatal("symlink accepted")
	}
}
func TestUsageCountsAllocatedBlocks(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	p := filepath.Join(dir, "archive.sqlite-wal")
	if e := os.WriteFile(p, make([]byte, 8193), 0600); e != nil {
		t.Fatal(e)
	}
	i, _ := os.Stat(p)
	usage, e := Usage([]string{dir})
	if e != nil {
		t.Fatal(e)
	}
	if usage[dir] != allocated(i) {
		t.Fatal(usage)
	}
}
func TestExportIndependentAndCollision(t *testing.T) {
	ctx := context.Background()
	s, e := archive.Open(filepath.Join(t.TempDir(), "live"), archive.DefaultOptions())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	source, e := s.Backup(ctx)
	if e != nil {
		t.Fatal(e)
	}
	dest := t.TempDir()
	os.Chmod(dest, 0700)
	if _, e = ExportBackup(ctx, source, filepath.Dir(source)); e == nil {
		t.Fatal("same destination accepted")
	}
	if _, e = ExportBackup(ctx, source, dest); e != nil {
		t.Fatal(e)
	}
	if _, e = ExportBackup(ctx, source, dest); e == nil {
		t.Fatal("overwrite accepted")
	}
	if _, e = os.Stat(source); e != nil {
		t.Fatal("source removed")
	}
}
func TestDiskPressurePreflight(t *testing.T) {
	if e := RequireSpace(t.TempDir(), ^uint64(0)); e == nil {
		t.Fatal("impossible allocation accepted")
	}
}
func TestMixedDataRootPreSchemaAndIdentity(t *testing.T) {
	ctx := context.Background()
	s, e := archive.Open(filepath.Join(t.TempDir(), "live"), archive.DefaultOptions())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	first, e := s.Backup(ctx)
	if e != nil {
		t.Fatal(e)
	}
	root := filepath.Dir(filepath.Dir(first))
	pre := filepath.Join(root, "pre-schema-7-old.sqlite")
	if e = os.Rename(first, pre); e != nil {
		t.Fatal(e)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(pre, old, old)
	if _, e = archive.CompressBackup(ctx, pre, "", archive.DefaultMaxBackupBytes); e != nil {
		t.Fatal(e)
	}
	newest, e := s.Backup(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(newest, filepath.Join(root, "pre-schema-7-new.sqlite")); e != nil {
		t.Fatal(e)
	}
	// Existing backups directory, live database and WAL must not be candidates.
	plan, e := PlanRetention(ctx, root, Policy{KeepRaw: 1}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Path != pre {
		t.Fatal(plan)
	}
	c := plan.Candidates[0]
	if e = os.Chmod(pre, 0640); e != nil {
		t.Fatal(e)
	}
	if e = unchanged(pre, c.identities); e == nil {
		t.Fatal("ctime mutation missed")
	}
	if _, e = os.Stat(filepath.Join(root, "archive.sqlite")); e != nil {
		t.Fatal(e)
	}
}
func TestOversizedManifestAndCancelledLock(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	path := filepath.Join(dir, "manifest")
	if e := os.WriteFile(path, make([]byte, 16385), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := readManifest(path); e == nil {
		t.Fatal("oversized manifest accepted")
	}
	lock, e := directoryLock(context.Background(), dir, true)
	if e != nil {
		t.Fatal(e)
	}
	defer unlock(lock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if f, e := directoryLock(ctx, dir, true); e == nil {
		unlock(f)
		t.Fatal("cancelled lock acquired")
	}
}
