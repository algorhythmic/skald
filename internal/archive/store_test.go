package archive

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

var testNow = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func testStore(t *testing.T) (*Store, Registration, []byte) {
	t.Helper()
	opts := DefaultOptions()
	opts.MinFreeBytes = 0
	s, err := Open(filepath.Join(t.TempDir(), "data"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r := Registration{Source: sessioncapture.Source{Namespace: "fixture:claude", Provider: sessioncapture.Claude, StreamID: "fixture"}, Root: t.TempDir(), Path: "session.jsonl"}
	if err := s.Register(context.Background(), []Registration{r}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../testdata/claude/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return s, r, b
}
func testBatch(t *testing.T, b []byte, r Registration, cp sessioncapture.Checkpoint) sessioncapture.Batch {
	t.Helper()
	out, err := sessioncapture.Read(bytes.NewReader(b), r.Source, cp, sessioncapture.DefaultLimits(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func mustIngest(t *testing.T, s *Store, r Registration, cp sessioncapture.Checkpoint, b sessioncapture.Batch) {
	t.Helper()
	if err := s.Ingest(context.Background(), r, cp, b, testNow); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveRestartDuplicateAndSourceRemoval(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	before, _ := s.Stats(ctx, []string{r.Namespace})
	if err := s.Ingest(ctx, r, sessioncapture.Checkpoint{}, batch, testNow); !errors.Is(err, ErrCheckpoint) {
		t.Fatal("stale ingest must not overwrite committed checkpoint", err)
	}
	replay := testBatch(t, raw, r, batch.Checkpoint)
	mustIngest(t, s, r, batch.Checkpoint, replay)
	after, _ := s.Stats(ctx, []string{r.Namespace})
	if before["record_versions"] != after["record_versions"] || before["boundary"] != after["boundary"] {
		t.Fatal("retry published duplicate changes")
	}
	if err := s.SetHealth(ctx, r, "unavailable", testNow); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.Close()
	reopened, err := Open(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	cp, err := reopened.Checkpoint(ctx, r.Key())
	if err != nil || cp != batch.Checkpoint {
		t.Fatal("checkpoint lost on restart", err)
	}
	original := batch.Records[3]
	exact, err := reopened.Get(ctx, []string{r.Namespace}, original.Record.RecordKey, original.Record.SourceRevision, original.Record.AdapterVersion, true)
	if err != nil || !bytes.Equal(exact.Raw, original.Raw) || exact.Record.Kind != "native_recap" {
		t.Fatal("archive lost original source", err)
	}
	sessions, err := reopened.Sessions(ctx, []string{r.Namespace}, "", 25)
	if err != nil || len(sessions.Items) != 1 || sessions.Items[0].SourceHealth != "unavailable" || sessions.Items[0].Activity != "unknown" {
		t.Fatal("source loss fabricated lifecycle", err)
	}
}

func TestCheckpointFailureRollsBackAllWrites(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	first := testBatch(t, raw[:bytes.IndexByte(raw, '\n')+1], r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, first)
	before, _ := s.Stats(ctx, []string{r.Namespace})
	if _, err := s.db.Exec(`CREATE TRIGGER fail_checkpoint BEFORE UPDATE ON ingest_checkpoints BEGIN SELECT RAISE(ABORT,'injected commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	tail := testBatch(t, raw, r, first.Checkpoint)
	if err := s.Ingest(ctx, r, first.Checkpoint, tail, testNow); err == nil {
		t.Fatal("injected failure ignored")
	}
	cp, _ := s.Checkpoint(ctx, r.Key())
	after, _ := s.Stats(ctx, []string{r.Namespace})
	if cp != first.Checkpoint || before["record_versions"] != after["record_versions"] || before["boundary"] != after["boundary"] {
		t.Fatal("partial transaction committed")
	}
	var contents int
	if err := s.db.QueryRow("SELECT count(*) FROM content_objects").Scan(&contents); err != nil || contents != 1 {
		t.Fatal("orphaned content after failed commit", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_checkpoint"); err != nil {
		t.Fatal(err)
	}
	mustIngest(t, s, r, first.Checkpoint, tail)
}

func TestRevisionHistoryScopeAndPagination(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	original := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, original)
	key := *original.Records[0].Record.ConversationKey
	page, err := s.Artifacts(ctx, []string{r.Namespace}, key, "", 1)
	if err != nil || page.Next == "" {
		t.Fatal("missing page cursor", err)
	}
	if _, err := s.Artifacts(ctx, []string{"other"}, key, page.Next, 1); err == nil {
		t.Fatal("cross-scope cursor accepted")
	}
	if _, err := s.Get(ctx, []string{"other"}, original.Records[0].Record.RecordKey, original.Records[0].Record.SourceRevision, sessioncapture.AdapterVersion, true); err == nil {
		t.Fatal("cross-scope raw read accepted")
	}
	rewritten := bytes.Replace(raw, []byte("Keep archive"), []byte("Retain archive"), 1)
	revised := testBatch(t, rewritten, r, original.Checkpoint)
	mustIngest(t, s, r, original.Checkpoint, revised)
	if _, err := s.Artifacts(ctx, []string{r.Namespace}, key, page.Next, 1); err == nil || err.Error() != "cursor_expired" {
		t.Fatal("mixed-boundary pagination accepted", err)
	}
	old, err := s.Get(ctx, []string{r.Namespace}, original.Records[0].Record.RecordKey, original.Records[0].Record.SourceRevision, sessioncapture.AdapterVersion, true)
	if err != nil || !bytes.Equal(old.Raw, original.Records[0].Raw) {
		t.Fatal("source revision overwritten", err)
	}
	current, err := s.Get(ctx, []string{r.Namespace}, revised.Records[0].Record.RecordKey, revised.Records[0].Record.SourceRevision, sessioncapture.AdapterVersion, true)
	if err != nil || bytes.Equal(current.Raw, old.Raw) {
		t.Fatal("new revision not distinct", err)
	}
}

func TestBackupRestoreWithoutNativeSources(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	backup, err := s.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err := Restore(ctx, backup, target, DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, backup, target, DefaultOptions()); err == nil {
		t.Fatal("restore overwrote existing destination")
	}
	restored, err := Open(target, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.instance == s.instance {
		t.Fatal("restore reused live archive instance")
	}
	cp, err := restored.Checkpoint(ctx, r.Key())
	if err != nil || cp != batch.Checkpoint {
		t.Fatal("restore lost checkpoint", err)
	}
	for _, item := range batch.Records {
		got, err := restored.Get(ctx, []string{r.Namespace}, item.Record.RecordKey, item.Record.SourceRevision, item.Record.AdapterVersion, true)
		if err != nil || !bytes.Equal(got.Raw, item.Raw) {
			t.Fatal("restore changed original bytes", err)
		}
	}
}

func TestBackupTamperingIsRefused(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	b := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, b)
	backup, err := s.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", DSN(backup, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP TRIGGER immutable_content; UPDATE content_objects SET original_bytes=zeroblob(byte_length)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	target := filepath.Join(t.TempDir(), "invalid")
	if err := Restore(ctx, backup, target, DefaultOptions()); err == nil || err.Error() != "backup_digest_mismatch" {
		t.Fatal("corrupt backup accepted", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("invalid restore created destination")
	}
}

func TestExclusiveWriterAliasesAndFutureSchema(t *testing.T) {
	ctx := context.Background()
	s, r, _ := testStore(t)
	if other, err := Open(s.dir, DefaultOptions()); err == nil {
		other.Close()
		t.Fatal("second writer acquired archive")
	}
	moved := r
	moved.Root = t.TempDir()
	if err := s.Register(ctx, []Registration{moved}); err == nil {
		t.Fatal("silent namespace relocation")
	}
	moved.RootAliases = []string{r.Root}
	if err := s.Register(ctx, []Registration{moved}); err != nil {
		t.Fatal("explicit alias refused", err)
	}
	bad := moved
	bad.Provider = sessioncapture.Codex
	if err := s.Register(ctx, []Registration{bad}); err == nil {
		t.Fatal("namespace provider identity changed")
	}
	dir := s.dir
	if _, err := s.db.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if reopened, err := Open(dir, DefaultOptions()); err == nil {
		reopened.Close()
		t.Fatal("future schema opened")
	}
}

func TestCapacityAndSuppressionDoNotAdvanceCursor(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	b := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	s.options.MinFreeBytes = 1 << 62
	if err := s.Ingest(ctx, r, sessioncapture.Checkpoint{}, b, testNow); !errors.Is(err, ErrCapacity) {
		t.Fatal("capacity not enforced", err)
	}
	s.options.MinFreeBytes = 0
	if _, err := s.db.Exec("INSERT INTO purge_tombstones VALUES (?,'conversation',?,1,?)", r.Namespace, sessionrecord.Conversation(r.Namespace, b.Checkpoint.ConversationID), stamp(testNow)); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, r, sessioncapture.Checkpoint{}, b, testNow); err == nil || err.Error() != "capture_suppressed" {
		t.Fatal("suppressed source resurrected", err)
	}
	cp, _ := s.Checkpoint(ctx, r.Key())
	if cp.Version != 0 {
		t.Fatal("failed capture advanced checkpoint")
	}
}

func TestSchemaOneUpgradeHasRecoveryCopy(t *testing.T) {
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
	ddl, _ := migrations.ReadFile("migrations/001_capture.sql")
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	backups, err := filepath.Glob(filepath.Join(dir, "pre-schema-2-*.sqlite"))
	if err != nil || len(backups) != 1 {
		t.Fatal("migration has no recovery copy", err)
	}
	old, err := sql.Open("sqlite", DSN(backups[0], true))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var version int
	if err := old.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatal("backup is not pre-upgrade schema", err)
	}
	if s.instance == "" {
		t.Fatal("interrupted initial metadata was not recovered")
	}
}

func TestNativeTitleProjectionUsesSourceOrderAndLabelsAmbiguity(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	first := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, first)
	title := []byte("{\"type\":\"ai-title\",\"sessionId\":\"synthetic-claude\",\"timestamp\":\"2020-01-01T00:00:00Z\",\"title\":\"Later native title\"}\n")
	next := testBatch(t, append(append([]byte{}, raw...), title...), r, first.Checkpoint)
	mustIngest(t, s, r, first.Checkpoint, next)
	page, err := s.Sessions(ctx, []string{r.Namespace}, "", 25)
	if err != nil {
		t.Fatal(err)
	}
	if page.Items[0].Title != "Later native title" || page.Items[0].TitleRef == nil || page.Items[0].TitleOrderingAmbiguous {
		t.Fatal("title selection ignored native order")
	}
	other := r
	other.StreamID = "other-surface"
	other.Path = "other.jsonl"
	if err := s.Register(ctx, []Registration{r, other}); err != nil {
		t.Fatal(err)
	}
	conflicting := bytes.Replace(title, []byte("Later native title"), []byte("Different stream title"), 1)
	batch := testBatch(t, conflicting, other, sessioncapture.Checkpoint{})
	mustIngest(t, s, other, sessioncapture.Checkpoint{}, batch)
	page, err = s.Sessions(ctx, []string{r.Namespace}, "", 25)
	if err != nil {
		t.Fatal(err)
	}
	if page.Items[0].Title != "Later native title" || !page.Items[0].TitleOrderingAmbiguous {
		t.Fatal("cross-stream title order was invented")
	}
}
