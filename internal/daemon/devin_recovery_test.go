package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/algorhythmic/skald/capturespool"
	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/internal/observability"
	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
	_ "modernc.org/sqlite"
)

func TestDevinHotWalBeforeFirstCheckpointRecovers(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	name := "synthetic-devin.db"
	path := filepath.Join(root, name)
	db, err := sql.Open("sqlite", archive.DSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)",
		"CREATE TABLE messages (position INTEGER PRIMARY KEY, kind TEXT NOT NULL, payload TEXT NOT NULL)",
		`INSERT INTO meta VALUES ('schema_version','1'),('info','{"title":"Synthetic recovery"}')`,
		`INSERT INTO messages VALUES (0,'agent_message','{"content":[{"content":{"type":"text","text":"synthetic reply"}}]}')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	wal := path + "-wal"
	if _, err := os.Stat(wal); err != nil {
		t.Fatal("synthetic WAL was not created", err)
	}
	now := time.Now()
	if err := os.Chtimes(wal, now, now); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.MinFreeBytes = 0
	cfg.MaxDatabaseBytes = 64 << 20
	r := archive.Registration{Source: sessioncapture.Source{
		Namespace: "fixture:devin-wal", Provider: sessioncapture.Devin,
		StreamID: "synthetic-stream", ConversationID: "synthetic-devin",
	}, Root: root, Path: name}
	cfg.Sources = []archive.Registration{r}
	store, err := archive.Open(filepath.Join(t.TempDir(), "archive"), cfg.Options())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	spool, err := capturespool.Open(filepath.Join(t.TempDir(), "spool"), []string{localDestination}, capturespool.Limits{MaxPending: 16, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	collector := &Collector{Store: store, Spool: spool, Config: cfg, Metrics: observability.New()}
	if err := collector.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	collector.Scan(ctx)
	if issue := collector.Issues()[r.Key()]; issue != "" {
		t.Fatal("hot WAL did not defer cleanly", issue)
	}
	pending, err := store.Checkpoint(ctx, r.Key())
	if err != nil || pending.Version != 1 || pending.Offset != 0 || pending.Ordinal != 0 || pending.PrefixDigest != sessionrecord.Digest(nil) {
		t.Fatal("hot WAL did not establish valid empty checkpoint", pending, err)
	}
	producer, found, err := spool.Checkpoint(r.Key())
	if err != nil || !found || producer != pending {
		t.Fatal("spool and archive checkpoints differ after deferral", producer, pending, err)
	}
	stats, err := store.Stats(ctx, cfg.Namespaces())
	if err != nil || stats["record_versions"] != int64(0) {
		t.Fatal("hot WAL captured unquiet records", stats, err)
	}
	// A first non-growing probe preserves the active -> inactive edge. Later
	// bookkeeping touches must not create a fresh durable envelope every poll.
	for i := 0; i < 5; i++ {
		touch := time.Now().Add(time.Duration(i+1) * time.Millisecond)
		if err := os.Chtimes(wal, touch, touch); err != nil {
			t.Fatal(err)
		}
		collector.Scan(ctx)
	}
	if skipped := collector.Metrics.Snapshot().Counters["empty_polls_skipped"]; skipped != 4 {
		t.Fatal("bookkeeping-only WAL touches produced redundant deliveries", skipped)
	}
	if h, err := store.Health(ctx, cfg.Namespaces()); err != nil || len(h) != 1 || h[0].Health != "partial_line" || h[0].CheckedAt == nil {
		t.Fatal("suppressed hot polls lost current health", h, err)
	}
	quiet := time.Now().Add(-31 * time.Second)
	if err := os.Chtimes(wal, quiet, quiet); err != nil {
		t.Fatal(err)
	}
	collector.Scan(ctx)
	if issue := collector.Issues()[r.Key()]; issue != "" {
		t.Fatal("quiet WAL did not resume capture", issue)
	}
	captured, err := store.Checkpoint(ctx, r.Key())
	if err != nil || captured.Version != 1 || captured.Ordinal != 2 || captured.Offset <= pending.Offset {
		t.Fatal("quiet WAL lost canonical records", captured, err)
	}
	stats, err = store.Stats(ctx, cfg.Namespaces())
	if err != nil || stats["record_versions"] != int64(2) {
		t.Fatal("canonical records were not archived", stats, err)
	}
}
