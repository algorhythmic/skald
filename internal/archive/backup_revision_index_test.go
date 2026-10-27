package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupRevisionIndexBoundedBatches(t *testing.T) {
	const objects = 37
	const batch = 3
	idx, e := newRevisionIndex(context.Background(), filepath.Join(t.TempDir(), "backup.sqlite"), backupLimits{Objects: objects, BatchRows: batch})
	if e != nil {
		t.Fatal("index open")
	}
	path := idx.path
	for n := 0; n < objects; n++ {
		old := sessionrecord.Digest([]byte(fmt.Sprintf("old-%d", n)))
		next := sessionrecord.Digest([]byte(fmt.Sprintf("new-%d", n)))
		if idx.put(old, next) != nil {
			t.Fatal("batch insertion")
		}
		if idx.peakBatch > batch {
			t.Fatal("batch grew beyond bound")
		}
	}
	for n := 0; n < objects; n++ {
		old := sessionrecord.Digest([]byte(fmt.Sprintf("old-%d", n)))
		want := sessionrecord.Digest([]byte(fmt.Sprintf("new-%d", n)))
		got, e := idx.get(old)
		if e != nil || got != want {
			t.Fatal("mapping lost across batches")
		}
	}
	if !errors.Is(idx.put("over-limit", "over-limit"), ErrBackupObjectLimit) {
		t.Fatal("unstable object refusal")
	}
	if info, e := os.Stat(path); e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("index not private")
	}
	idx.close()
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("scratch index retained")
	}
}
func TestLogicalBackupSmallConfiguredLimit(t *testing.T) {
	s, r, _ := testStore(t)
	var input bytes.Buffer
	for n := 0; n < 17; n++ {
		raw, _ := json.Marshal(map[string]any{"type": "user", "sessionId": "synthetic-claude", "uuid": fmt.Sprintf("scale-%d", n), "message": map[string]string{"role": "user", "content": "ordinary synthetic body"}})
		input.Write(raw)
		input.WriteByte('\n')
	}
	batch := testBatch(t, input.Bytes(), r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.sqlite")
	if e := logicalBackupWithLimits(context.Background(), s.db, ok, backupLimits{Objects: 17, BatchRows: 2}); e != nil {
		t.Fatal("bounded backup failed")
	}
	if verifyFile(context.Background(), ok, false) != nil {
		t.Fatal("bounded backup invalid")
	}
	fail := filepath.Join(dir, "refused.sqlite")
	if e := logicalBackupWithLimits(context.Background(), s.db, fail, backupLimits{Objects: 16, BatchRows: 2}); !errors.Is(e, ErrBackupObjectLimit) {
		t.Fatal("limit refusal not stable")
	}
	files, _ := filepath.Glob(filepath.Join(dir, ".redaction-index-*"))
	if len(files) != 0 {
		t.Fatal("scratch files retained")
	}
}
