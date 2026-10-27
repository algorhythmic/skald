package retrieval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/internal/archive"
)

func cacheTestEngine(t *testing.T) (*Engine, Config, *archive.Store) {
	t.Helper()
	opts := archive.DefaultOptions()
	opts.MinFreeBytes = 0
	store, err := archive.Open(filepath.Join(t.TempDir(), "archive"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	config := Config{Executable: "/unused-explicit-provider", SHA256: strings.Repeat("0", 64), Directory: filepath.Join(t.TempDir(), "private-cache")}
	engine, err := New(config, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine, config, store
}
func cacheTestPublication(t *testing.T, e *Engine) string {
	t.Helper()
	dir, err := os.MkdirTemp(e.config.Directory, "publication-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.sqlite"), []byte("synthetic withdrawn plaintext"), 0600); err != nil {
		t.Fatal(err)
	}
	e.entries["test"] = &publication{directory: dir}
	return dir
}
func TestInvalidationPurgesBeforeMutation(t *testing.T) {
	e, _, _ := cacheTestEngine(t)
	dir := cacheTestPublication(t, e)
	called := false
	err := e.WithInvalidation(func() error {
		called = true
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("cached plaintext survived into mutation")
		}
		return nil
	})
	if err != nil || !called || len(e.entries) != 0 {
		t.Fatalf("invalidation failed: %v", err)
	}
}
func TestInvalidationCleanupFailureRejectsMutation(t *testing.T) {
	e, _, _ := cacheTestEngine(t)
	dir := cacheTestPublication(t, e)
	external := filepath.Join(t.TempDir(), "must-remain")
	if err := os.WriteFile(external, []byte("external"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "index.sqlite-wal")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := e.WithInvalidation(func() error { called = true; return nil }); err == nil || called {
		t.Fatal("mutation ran despite unsafe cleanup")
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatal("symlink target changed")
	}
	if _, err := e.Query(context.Background(), []string{"synthetic"}, 0, Request{Text: "test"}); err == nil {
		t.Fatal("query continued after cleanup failure")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := e.WithInvalidation(nil); err != nil {
		t.Fatal(err)
	}
}
func TestExclusiveLeaseAndStartupOrphanPurge(t *testing.T) {
	e, config, store := cacheTestEngine(t)
	dir := cacheTestPublication(t, e)
	if other, err := New(config, store); err == nil {
		other.Close()
		t.Fatal("second engine acquired live cache")
	}
	// Simulate a process disappearing without its cleanup path.
	e.cacheRoot.Close()
	e.lease.Close()
	e.lease = nil
	restarted, err := New(config, store)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("startup retained orphan plaintext")
	}
	entries, err := os.ReadDir(config.Directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("startup retained cache allocations")
	}
}
func TestStartupRejectsUnsafeOrphanAndReleasesLease(t *testing.T) {
	e, config, store := cacheTestEngine(t)
	dir := cacheTestPublication(t, e)
	e.cacheRoot.Close()
	e.lease.Close()
	e.lease = nil
	link := filepath.Join(dir, "index.sqlite-wal")
	if err := os.Symlink("/tmp", link); err != nil {
		t.Fatal(err)
	}
	if restarted, err := New(config, store); err == nil {
		restarted.Close()
		t.Fatal("unsafe orphan accepted")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(config, store)
	if err != nil {
		t.Fatalf("failed startup leaked lease: %v", err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestCacheRejectsSymlinkAndPublicDirectory(t *testing.T) {
	e, config, store := cacheTestEngine(t)
	e.Close()
	config.Directory = filepath.Join(t.TempDir(), "link")
	target := t.TempDir()
	if err := os.Symlink(target, config.Directory); err != nil {
		t.Fatal(err)
	}
	if other, err := New(config, store); err == nil {
		other.Close()
		t.Fatal("cache symlink accepted")
	}
	config.Directory = filepath.Join(t.TempDir(), "public")
	if err := os.Mkdir(config.Directory, 0755); err != nil {
		t.Fatal(err)
	}
	// Mkdir's mode is filtered by the process umask. Establish the deliberately
	// public fixture explicitly, including under a private (0077) test runner.
	if err := os.Chmod(config.Directory, 0755); err != nil {
		t.Fatal(err)
	}
	if other, err := New(config, store); err == nil {
		other.Close()
		t.Fatal("public cache accepted")
	}
}
