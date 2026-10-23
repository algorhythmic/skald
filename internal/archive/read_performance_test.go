package archive

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

func queryPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "\n")
}

func TestVersionCountsUseCoveringScopeIndexes(t *testing.T) {
	s, _, _ := testStore(t)
	// Multiple namespaces caused the old query to scan every version, then
	// fetch the artifact table for each row. Check the actual production SQL.
	plan := queryPlan(t, s.db, scopedVersionCountSQL+"?,?,?,?)", "one", "two", "three", "four")
	for _, want := range []string{
		"SEARCH a USING COVERING INDEX artifacts_namespace (namespace=?)",
		"SEARCH v USING COVERING INDEX sqlite_autoindex_artifact_versions_1 (record_key=?)",
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("status count must scope artifacts before searching versions; missing %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "SCAN ") {
		t.Fatalf("scoped status count scans an entire table/index:\n%s", plan)
	}
	// Sessions already has a suitable covering index. Guard against turning
	// its correlated count into a full archive scan for each displayed row.
	plan = queryPlan(t, s.db, "SELECT ("+sessionVersionCountSQL+") FROM sessions s WHERE s.conversation_key=?", "conversation")
	for _, want := range []string{
		"SEARCH a USING COVERING INDEX artifacts_conversation (conversation_key=?)",
		"SEARCH v USING COVERING INDEX sqlite_autoindex_artifact_versions_1 (record_key=?)",
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("session count must search conversation versions; missing %q:\n%s", want, plan)
		}
	}
}

func schemaSixFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "archive.sqlite")
	if err := regularPrivate(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", DSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, name := range []string{"001_capture.sql", "002_daemon.sql", "003_titles.sql", "004_activity.sql", "005_continuation.sql", "006_delivery.sql"} {
		ddl, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatal(name, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO archive_meta VALUES(1,6,'pre-seven-instance',1,'restored-origin')`,
		`INSERT INTO sources VALUES('fixture','claude','synthetic','[]','{}')`,
		`INSERT INTO artifacts VALUES('record','fixture','source','fixture',NULL)`,
		`INSERT INTO content_objects VALUES('` + sessionrecord.Digest([]byte("a")) + `',1,'text/plain',X'61')`,
		`INSERT INTO artifact_versions VALUES('record','` + sessionrecord.Digest([]byte("a")) + `','2026-10-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSchemaSixUpgradePreservesRecoveryCopyAndCounts(t *testing.T) {
	dir := schemaSixFixture(t)
	s, err := Open(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	var instance, restored string
	if err := s.db.QueryRow("SELECT schema_version,archive_instance,restored_from FROM archive_meta").Scan(&version, &instance, &restored); err != nil || version != SchemaVersion || instance != "pre-seven-instance" || restored != "restored-origin" {
		t.Fatalf("metadata lost: %d %q %q %v", version, instance, restored, err)
	}
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal("schema version", version, err)
	}
	stats, err := s.Stats(context.Background(), []string{"fixture"})
	if err != nil || stats["record_versions"] != int64(1) || stats["sessions"] != int64(0) {
		t.Fatal("source-origin version count lost", stats, err)
	}
	backups, err := filepath.Glob(filepath.Join(dir, "pre-schema-7-*.sqlite"))
	if err != nil || len(backups) != 1 {
		t.Fatal("recovery backup missing", backups, err)
	}
	backup, err := sql.Open("sqlite", DSN(backups[0], true))
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if err := backup.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 6 {
		t.Fatal("backup was modified", version, err)
	}
	var count int
	if err := backup.QueryRow("SELECT count(*) FROM artifact_versions").Scan(&count); err != nil || count != 1 {
		t.Fatal("backup evidence missing", count, err)
	}
	if err := backup.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='artifacts_namespace'").Scan(&count); err != nil || count != 0 {
		t.Fatal("backup contains post-upgrade index", count, err)
	}
}

func TestSchemaSevenIndexBudgetFailurePreservesSchemaSixAndBackup(t *testing.T) {
	dir := schemaSixFixture(t)
	db, err := sql.Open("sqlite", DSN(filepath.Join(dir, "archive.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000)
 INSERT INTO artifacts SELECT printf('record:%0128d',x),'fixture','source','fixture',NULL FROM n`,
		"VACUUM",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	var pages, size int64
	if err := db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA page_size").Scan(&size); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	opts.MinFreeBytes = 0
	opts.MaxDatabaseBytes = pages * size
	if opts.MaxDatabaseBytes < 1<<20 {
		t.Fatal("fixture does not meet minimum archive budget")
	}
	if store, err := Open(dir, opts); !errors.Is(err, ErrCapacity) {
		if store != nil {
			store.Close()
		}
		t.Fatalf("index must respect the configured page budget: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(dir, "pre-schema-7-*.sqlite"))
	if err != nil || len(backups) != 1 {
		t.Fatal("recovery backup missing", backups, err)
	}
	for _, path := range []string{filepath.Join(dir, "archive.sqlite"), backups[0]} {
		if err := verifyFile(context.Background(), path, false); err != nil {
			t.Fatal("archive integrity after capacity failure", path, err)
		}
		db, err := sql.Open("sqlite", DSN(path, true))
		if err != nil {
			t.Fatal(err)
		}
		var version, artifacts, versions, indexes int
		if err := db.QueryRow("SELECT (SELECT schema_version FROM archive_meta),(SELECT count(*) FROM artifacts),(SELECT count(*) FROM artifact_versions),(SELECT count(*) FROM sqlite_master WHERE name='artifacts_namespace')").Scan(&version, &artifacts, &versions, &indexes); err != nil || version != 6 || artifacts != 10001 || versions != 1 || indexes != 0 {
			t.Fatalf("partial migration survived: %s schema=%d artifacts=%d versions=%d indexes=%d: %v", path, version, artifacts, versions, indexes, err)
		}
		db.Close()
	}
	// Raising the budget allows the same intact archive to upgrade normally.
	store, err := Open(dir, DefaultOptions())
	if err != nil {
		t.Fatal("retry after increasing capacity", err)
	}
	store.Close()
}

func TestSchemaSevenMigrationChecksFreeSpaceBeforeBackup(t *testing.T) {
	dir := schemaSixFixture(t)
	opts := DefaultOptions()
	opts.MinFreeBytes = ^uint64(0)
	if store, err := Open(dir, opts); !errors.Is(err, ErrCapacity) {
		if store != nil {
			store.Close()
		}
		t.Fatalf("migration must reserve backup/build disk space: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(dir, "pre-schema-7-*.sqlite"))
	if err != nil || len(backups) != 0 {
		t.Fatal("space preflight ran after backup", backups, err)
	}
}

func TestScopedVersionCountsSurviveRevisionReplayRedactionRestore(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	versions := make(map[sessionrecord.Ref]bool)
	for _, captured := range batch.Records {
		versions[sessionrecord.Ref{RecordKey: captured.Record.RecordKey, SourceRevision: captured.Record.SourceRevision}] = true
	}
	initial, err := s.Stats(ctx, []string{r.Namespace})
	if err != nil || initial["record_versions"] != int64(len(batch.Records)) {
		t.Fatal("initial complete-scope count", initial, err)
	}
	other := r
	other.Namespace = "fixture:other"
	if err := s.Register(ctx, []Registration{r, other}); err != nil {
		t.Fatal(err)
	}
	otherBatch := testBatch(t, raw, other, sessioncapture.Checkpoint{})
	mustIngest(t, s, other, sessioncapture.Checkpoint{}, otherBatch)

	check := func(store *Store, own, foreign int64) {
		t.Helper()
		for _, tc := range []struct {
			ns   []string
			want int64
		}{
			{nil, 0}, {[]string{"missing"}, 0},
			{[]string{r.Namespace}, own}, {[]string{other.Namespace}, foreign},
			{[]string{r.Namespace, r.Namespace}, own},
			{[]string{r.Namespace, r.Namespace, "missing"}, own},
			{[]string{r.Namespace, other.Namespace}, own + foreign},
			{[]string{r.Namespace, other.Namespace, "missing"}, own + foreign},
		} {
			stats, err := store.Stats(ctx, tc.ns)
			if err != nil || stats["record_versions"] != tc.want {
				t.Fatalf("scope %v: got %v, want %d: %v", tc.ns, stats, tc.want, err)
			}
		}
		for _, tc := range []struct {
			ns   string
			want int64
		}{{r.Namespace, own}, {other.Namespace, foreign}} {
			page, err := store.Sessions(ctx, []string{tc.ns}, "", 100)
			if err != nil || len(page.Items) != 1 || page.Items[0].RecordVersions != tc.want {
				t.Fatalf("session scope %q: got %+v, want %d: %v", tc.ns, page, tc.want, err)
			}
		}
	}
	own, foreign := int64(len(batch.Records)), int64(len(otherBatch.Records))
	check(s, own, foreign)
	rewritten := bytes.Replace(raw, []byte("Keep archive"), []byte("Retain archive"), 1)
	revised := testBatch(t, rewritten, r, batch.Checkpoint)
	mustIngest(t, s, r, batch.Checkpoint, revised)
	// Native-ID revisions retain both versions; ordinal identities can also
	// change when a source rewrite starts a new generation.
	for _, captured := range revised.Records {
		versions[sessionrecord.Ref{RecordKey: captured.Record.RecordKey, SourceRevision: captured.Record.SourceRevision}] = true
	}
	own = int64(len(versions))
	check(s, own, foreign)
	replay := testBatch(t, rewritten, r, revised.Checkpoint)
	mustIngest(t, s, r, revised.Checkpoint, replay)
	check(s, own, foreign)
	receipt, err := s.Redact(ctx, r.Namespace, "record", batch.Records[0].Record.RecordKey)
	if err != nil || receipt.VersionsRemoved == 0 {
		t.Fatalf("redaction: %+v %v", receipt, err)
	}
	own -= receipt.VersionsRemoved
	check(s, own, foreign)
	// Retirement changes active enrollment, not the historical namespace scope.
	// Restricting reads to the remaining source must still exclude old records.
	if err := s.Register(ctx, []Registration{r}); err != nil {
		t.Fatal(err)
	}
	check(s, own, foreign)
	backup, err := s.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if err := Restore(ctx, backup, destination, DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(destination, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	check(restored, own, foreign)
}

// This models a 187,373-version archive without transcript bodies. Both cases
// use the real schema, and the baseline removes only the new schema-7 index.
func BenchmarkScopedVersionCount(b *testing.B) {
	for _, optimized := range []bool{false, true} {
		name := "schema6"
		if optimized {
			name = "schema7"
		}
		b.Run(name, func(b *testing.B) {
			opts := DefaultOptions()
			opts.MinFreeBytes = 0
			s, err := Open(filepath.Join(b.TempDir(), "archive"), opts)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			if !optimized {
				if _, err := s.db.Exec("DROP INDEX artifacts_namespace"); err != nil {
					b.Fatal(err)
				}
			}
			for _, stmt := range []string{
				`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<3)
 INSERT INTO sources SELECT 'ns:'||x,'claude','synthetic','[]','{}' FROM n`,
				`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<199)
 INSERT INTO sessions SELECT printf('sr1:conversation:%064x',x),'ns:'||(x%4),printf('native:%d',x),'unknown' FROM n`,
				`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<99999)
 INSERT INTO artifacts SELECT printf('sr1:record:%064x',(x*104729)%100000),'ns:'||(x%4),'conversation',
 printf('sr1:conversation:%064x',x%200),printf('sr1:conversation:%064x',x%200) FROM n`,
				`INSERT INTO content_objects VALUES ('` + sessionrecord.Digest([]byte("a")) + `',1,'text/plain',X'61'),('` + sessionrecord.Digest([]byte("b")) + `',1,'text/plain',X'62')`,
				`INSERT INTO artifact_versions SELECT record_key,'` + sessionrecord.Digest([]byte("a")) + `','2026-10-01T00:00:00Z' FROM artifacts`,
				`INSERT INTO artifact_versions SELECT record_key,'` + sessionrecord.Digest([]byte("b")) + `','2026-10-02T00:00:00Z' FROM artifacts LIMIT 87373`,
			} {
				if _, err := s.db.Exec(stmt); err != nil {
					b.Fatal(err)
				}
			}
			for _, scopes := range []int{1, 4} {
				b.Run(fmt.Sprintf("namespaces%d", scopes), func(b *testing.B) {
					ns := []string{"ns:0", "ns:1", "ns:2", "ns:3"}[:scopes]
					in, args := scopeSQL(ns)
					query := "SELECT count(*) FROM artifact_versions v JOIN artifacts a USING(record_key) WHERE a.namespace IN (" + in + ")"
					b.ResetTimer()
					for b.Loop() {
						var count int64
						var err error
						if optimized {
							count, err = s.recordVersionCount(context.Background(), in, args)
						} else {
							err = s.db.QueryRow(query, args...).Scan(&count)
						}
						if err != nil || count < 1 {
							b.Fatal(count, err)
						}
					}
				})
			}
		})
	}
}
