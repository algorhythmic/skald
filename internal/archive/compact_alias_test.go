package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
)

func TestCompactOfflineRefusesSiblingHardlinkBeforeOpeningArchive(t *testing.T) {
	ctx := context.Background()
	s, source, raw := testStore(t)
	batch := testBatch(t, raw, source, sessioncapture.Checkpoint{})
	mustIngest(t, s, source, sessioncapture.Checkpoint{}, batch)
	if err := s.checkpointTruncate(ctx); err != nil {
		t.Fatal(err)
	}
	// The alias has no writer.lock or WAL. Its inode belongs to the live
	// archive, whose own directory holds the actual exclusive writer lease.
	aliasDir := t.TempDir()
	if err := os.Chmod(aliasDir, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(aliasDir, "archive.sqlite")
	original := filepath.Join(s.dir, "archive.sqlite")
	if err := os.Link(original, alias); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, apply := range []bool{false, true} {
		var err error
		if apply {
			_, err = CompactOffline(ctx, aliasDir, s.options)
		} else {
			_, err = PreviewCompaction(aliasDir, s.options)
		}
		if err == nil || err.Error() != "private_owned_file_required" {
			t.Fatalf("hardlinked archive accepted (apply=%v): %v", apply, err)
		}
	}
	after, err := os.ReadFile(original)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused compaction changed the live archive", err)
	}
	entries, err := os.ReadDir(aliasDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "archive.sqlite" {
		t.Fatalf("refused compaction opened or created archive sidecars/lock: %v %v", entries, err)
	}
	if cp, err := s.Checkpoint(ctx, source.Key()); err != nil || cp != batch.Checkpoint {
		t.Fatalf("live archive stopped serving its durable checkpoint: %+v %v", cp, err)
	}
}
