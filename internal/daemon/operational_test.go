package daemon

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOperationalStorageCountsBlocksAndRefusesSymlinks(t *testing.T) {
	root := t.TempDir()
	if e := os.Mkdir(filepath.Join(root, "spool"), 0700); e != nil {
		t.Fatal(e)
	}
	var total int64
	for _, name := range []string{"archive.sqlite", "archive.sqlite-wal", "pre-schema-8-test.sqlite", "spool/test.sqlite"} {
		path := filepath.Join(root, name)
		if e := os.WriteFile(path, make([]byte, 8193), 0600); e != nil {
			t.Fatal(e)
		}
		i, e := os.Stat(path)
		if e != nil {
			t.Fatal(e)
		}
		total += i.Sys().(*syscall.Stat_t).Blocks * 512
	}
	values, e := sampleStorage(root)
	if e != nil || values["data_allocated_bytes"] != total || values["spool_allocated_bytes"] == 0 || values["recovery_allocated_bytes"] == 0 {
		t.Fatal(values, e)
	}
	if e := os.Remove(filepath.Join(root, "archive.sqlite-wal")); e != nil {
		t.Fatal(e)
	}
	values, e = sampleStorage(root)
	if e != nil || values["wal_allocated_bytes"] != 0 {
		t.Fatal("removed WAL remained allocated", values, e)
	}
	if e := os.Symlink(t.TempDir(), filepath.Join(root, "outside")); e != nil {
		t.Fatal(e)
	}
	if _, e = sampleStorage(root); e == nil {
		t.Fatal("followed external storage symlink")
	}
}
