package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

func inflateCompactionFixture(t *testing.T, s *Store, objects int) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < objects; i++ {
		raw := []byte(strings.Repeat("synthetic retained archive payload ", 2000) + NewID())
		if _, err = tx.Exec("INSERT INTO content_objects VALUES(?,?,'application/json',?)", sessionrecord.Digest(raw), len(raw), raw); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.checkpointTruncate(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestCompactionPreviewIsReadOnlyAndRequiresExistingArchive(t *testing.T) {
	s, _, _ := testStore(t)
	inflateCompactionFixture(t, s, 10)
	before, err := os.ReadFile(filepath.Join(s.dir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	entriesBefore, _ := os.ReadDir(s.dir)
	preview, err := PreviewCompaction(s.dir, s.options)
	if err != nil || preview.DatabaseBytes == 0 || preview.SchemaVersion != 8 {
		t.Fatal(preview, err)
	}
	after, err := os.ReadFile(filepath.Join(s.dir, "archive.sqlite"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("preview modified database", err)
	}
	entriesAfter, _ := os.ReadDir(s.dir)
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatal("preview created files")
	}
	if _, err = CompactOffline(context.Background(), s.dir, s.options); err == nil || err.Error() != "archive_in_use" {
		t.Fatal("active writer accepted", err)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err = PreviewCompaction(missing, DefaultOptions()); err == nil {
		t.Fatal("missing archive accepted")
	}
	if _, err = os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("preview created directory")
	}
}
func TestCompressThenCompactPreservesEvidenceAndRecovery(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	inflateCompactionFixture(t, s, 100)
	original, err := PreviewCompaction(s.dir, s.options)
	if err != nil {
		t.Fatal(err)
	}
	for {
		v, e := s.CompressPayloads(ctx, 100, 64<<20)
		if e != nil {
			t.Fatal(e)
		}
		if v.Examined == 0 {
			break
		}
	}
	if err = s.checkpointTruncate(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := PreviewCompaction(s.dir, s.options)
	if err != nil || before.EstimatedReusableBytes < 1<<20 {
		t.Fatal("fixture has no free pages", before, err)
	}
	var instance string
	if err = s.db.QueryRow("SELECT archive_instance FROM archive_meta").Scan(&instance); err != nil {
		t.Fatal(err)
	}
	dir, options := s.dir, s.options
	s.Close()
	recovery := filepath.Join(dir, "pre-schema-8-retained.sqlite")
	sentinel := []byte("retained rollback must never be removed")
	if err = os.WriteFile(recovery, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := CompactOffline(ctx, dir, options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.After == nil || result.ReclaimedDatabaseBytes <= 1<<20 || result.After.DatabaseBytes >= original.DatabaseBytes {
		t.Fatal("database not reclaimed", result)
	}
	if got, _ := os.ReadFile(recovery); !bytes.Equal(got, sentinel) {
		t.Fatal("recovery file changed")
	}
	reopened, err := Open(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.instance != instance {
		t.Fatal("archive identity changed")
	}
	for _, item := range batch.Records {
		got, err := reopened.Get(ctx, []string{r.Namespace}, item.Record.RecordKey, item.Record.SourceRevision, item.Record.AdapterVersion, true)
		if err != nil || !bytes.Equal(got.Raw, item.Raw) {
			t.Fatal("exact reference changed", err)
		}
	}
	if err = verifyFile(ctx, filepath.Join(dir, "archive.sqlite"), true); err != nil {
		t.Fatal(err)
	}
	t.Logf("synthetic raw payload database: before=%d allocated=%d after_compress=%d reusable=%d after_compact=%d allocated=%d", original.DatabaseBytes, original.DatabaseAllocatedBytes, before.DatabaseBytes, before.EstimatedReusableBytes, result.After.DatabaseBytes, result.After.DatabaseAllocatedBytes)
}
func TestCompactionCapacityAndCancellationPreserveDatabase(t *testing.T) {
	s, _, _ := testStore(t)
	inflateCompactionFixture(t, s, 100)
	path := filepath.Join(s.dir, "archive.sqlite")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(before)
	s.options.MinFreeBytes = ^uint64(0)
	if _, err = s.Compact(context.Background()); err != ErrCapacity {
		t.Fatal("headroom refusal", err)
	}
	after, _ := os.ReadFile(path)
	if sha256.Sum256(after) != digest {
		t.Fatal("headroom failure changed database")
	}
	s.options.MinFreeBytes = 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.Compact(ctx); err == nil {
		t.Fatal("cancellation accepted")
	}
	after, _ = os.ReadFile(path)
	if sha256.Sum256(after) != digest {
		t.Fatal("cancellation changed database")
	}
	// Interrupt the real SQLite VACUUM statement after it begins. SQLite must
	// leave a valid original archive or a fully committed compacted archive.
	ctx, cancel = context.WithTimeout(context.Background(), time.Microsecond*100)
	defer cancel()
	if _, err = s.db.ExecContext(ctx, "VACUUM"); err == nil {
		t.Fatal("VACUUM fixture finished before cancellation")
	}
	if err = s.checkpointTruncate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = verifyFile(context.Background(), path, true); err != nil {
		t.Fatal("interrupted VACUUM corrupted archive", err)
	}
}

// Resource limits are confined to a subprocess. EFBIG simulates a real storage
// write failure inside SQLite VACUUM without filling the developer's filesystem.
func TestCompactionDiskFailureSubprocess(t *testing.T) {
	dir := os.Getenv("SKALD_COMPACTION_FAILURE_DIR")
	if dir == "" {
		s, _, _ := testStore(t)
		inflateCompactionFixture(t, s, 100)
		dir = s.dir
		s.Close()
		cmd := exec.Command(os.Args[0], "-test.run=^TestCompactionDiskFailureSubprocess$", "-test.v")
		cmd.Env = append(os.Environ(), "SKALD_COMPACTION_FAILURE_DIR="+dir)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("disk failure child: %v\n%s", err, output)
		}
		t.Log(string(output))
		return
	}
	s, err := Open(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.checkpointTruncate(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "archive.sqlite")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(before)
	if _, err = s.db.Exec("PRAGMA temp_store=FILE"); err != nil {
		t.Fatal(err)
	}
	var old syscall.Rlimit
	if err = syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	limited := old
	limited.Cur = 1 << 20
	if err = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		t.Fatal(err)
	}
	result, compactErr := s.Compact(context.Background())
	if err = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if compactErr == nil || result.Applied {
		t.Fatal("storage write failure not observed", result, compactErr)
	}
	if err = s.checkpointTruncate(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(after) != digest {
		t.Fatal("failed VACUUM changed original database", err)
	}
	if err = verifyFile(context.Background(), path, true); err != nil {
		t.Fatal("failed VACUUM corrupted evidence", err)
	}
	s.Close()
	reopened, err := Open(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	t.Log("real file-size-limit write failure rolled back:", compactErr)
}
