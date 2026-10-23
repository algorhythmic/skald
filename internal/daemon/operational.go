package daemon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// sampleStorage stats only Skald-owned files. Work is bounded independently of
// the archive's payload volume; transient SQLite sidecar removal is harmless.
func sampleStorage(root string) (map[string]int64, error) {
	values := map[string]int64{"data_allocated_bytes": 0, "database_allocated_bytes": 0, "wal_allocated_bytes": 0, "spool_allocated_bytes": 0, "recovery_allocated_bytes": 0}
	files := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("storage_symlink_refused")
		}
		if entry.IsDir() {
			if len(strings.Split(rel, string(filepath.Separator))) > 4 {
				return errors.New("storage_inventory_limit")
			}
			return nil
		}
		files++
		if files > 4096 {
			return errors.New("storage_inventory_limit")
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("storage_stat_unavailable")
		}
		bytes := st.Blocks * 512
		values["data_allocated_bytes"] += bytes
		switch {
		case rel == "archive.sqlite":
			values["database_allocated_bytes"] += bytes
		case rel == "archive.sqlite-wal" || rel == "archive.sqlite-shm":
			values["wal_allocated_bytes"] += bytes
		case strings.HasPrefix(rel, "spool/"):
			values["spool_allocated_bytes"] += bytes
		case strings.HasPrefix(rel, "pre-schema-") || strings.HasPrefix(rel, "backups/") || strings.HasPrefix(rel, "deployment-backups/"):
			values["recovery_allocated_bytes"] += bytes
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var disk syscall.Statfs_t
	if err := syscall.Statfs(root, &disk); err != nil {
		return nil, err
	}
	values["filesystem_free_bytes"] = int64(disk.Bavail) * disk.Bsize
	return values, nil
}

func (c *Collector) operationalSample(ctx context.Context, root string) {
	bad := int64(0)
	if values, err := sampleStorage(root); err == nil {
		for name, value := range values {
			c.Metrics.Gauge(name, value)
		}
	} else {
		bad++
	}
	health, err := c.Store.OperationalHealth(ctx)
	if err != nil {
		bad++
	} else {
		for _, state := range []string{"available", "unavailable", "blocked", "capture_gaps"} {
			c.Metrics.Gauge("sources_"+state, health[state])
		}
	}
	if c.Spool != nil {
		if status, err := c.Spool.Status(); err == nil {
			c.Metrics.Gauge("capture_pending", int64(status.Pending))
			c.Metrics.Gauge("capture_pending_bytes", status.PendingBytes)
		} else {
			bad++
		}
	}
	c.Metrics.Gauge("operational_sample_errors", bad)
}

func (c *Collector) sampleOperations(ctx context.Context, root string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		sampleCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		c.operationalSample(sampleCtx, root)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
