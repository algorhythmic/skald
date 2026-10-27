package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/internal/observability"
	"github.com/algorhythmic/skald/sessioncapture"
)

func prefixFixture(t *testing.T) (archive.Registration, string, []byte) {
	t.Helper()
	root := t.TempDir()
	r := archive.Registration{Source: sessioncapture.Source{Namespace: "fixture:prefix", Provider: sessioncapture.Claude, StreamID: "synthetic-prefix", ConversationID: "synthetic-prefix"}, Root: root, Path: "source.jsonl"}
	data := []byte("{\"type\":\"user\",\"uuid\":\"one\",\"sessionId\":\"synthetic-prefix\",\"message\":{\"role\":\"user\",\"content\":\"aaaa\"}}\n" +
		"{\"type\":\"assistant\",\"uuid\":\"two\",\"sessionId\":\"synthetic-prefix\",\"message\":{\"role\":\"assistant\",\"content\":\"next\"}}\n")
	if err := os.WriteFile(filepath.Join(root, r.Path), data, 0600); err != nil {
		t.Fatal(err)
	}
	return r, filepath.Join(root, "prefix-checkpoints"), data
}

func prefixCapture(t *testing.T, r archive.Registration, cp sessioncapture.Checkpoint, dir string) sessioncapture.Batch {
	t.Helper()
	batch, err := captureFileWithPrefixState(context.Background(), r, cp, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func prefixLoad(t *testing.T, r archive.Registration, cp sessioncapture.Checkpoint, dir string) sessioncapture.PrefixState {
	t.Helper()
	root, err := openPrefixDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := os.Stat(filepath.Join(r.Root, r.Path))
	if err != nil {
		t.Fatal(err)
	}
	return loadFilePrefix(root, r.Source, cp, info)
}

func TestPrefixCheckpointRestartAndMutations(t *testing.T) {
	for _, mutation := range []string{"unchanged", "append", "partial", "truncate", "rewrite", "rotation"} {
		t.Run(mutation, func(t *testing.T) {
			r, dir, data := prefixFixture(t)
			first := prefixCapture(t, r, sessioncapture.Checkpoint{}, dir)
			if len(first.Records) != 2 || prefixLoad(t, r, first.Checkpoint, dir).Version != 1 {
				t.Fatal("successful capture did not persist resumable state")
			}
			file := filepath.Join(r.Root, r.Path)
			nextLine := []byte("{\"type\":\"user\",\"uuid\":\"three\",\"sessionId\":\"synthetic-prefix\",\"message\":{\"role\":\"user\",\"content\":\"appended\"}}\n")
			switch mutation {
			case "append", "rotation":
				data = append(data, nextLine...)
			case "partial":
				data = append(data, '{')
			case "truncate":
				data = data[:bytes.IndexByte(data, '\n')+1]
			case "rewrite":
				data = bytes.Replace(data, []byte("aaaa"), []byte("bbbb"), 1)
			}
			if mutation == "rotation" {
				if err := os.Rename(file, file+".previous"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			if mutation == "truncate" || mutation == "rotation" {
				if prefixLoad(t, r, first.Checkpoint, dir).Version != 0 {
					t.Fatal("identity change or truncation reused saved hash state")
				}
			}
			// Every invocation reopens its persisted state; no process-local cache
			// is involved, so this also exercises a restarted collector.
			got := prefixCapture(t, r, first.Checkpoint, dir)
			want, err := captureFile(context.Background(), r, first.Checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			if got.Checkpoint != want.Checkpoint || !reflect.DeepEqual(got.Gaps, want.Gaps) || got.Pending != want.Pending || got.Blocked != want.Blocked || got.More != want.More || len(got.Records) != len(want.Records) {
				t.Fatal("persisted state changed conservative capture semantics")
			}
			for i := range got.Records {
				if !bytes.Equal(got.Records[i].Raw, want.Records[i].Raw) {
					t.Fatal("persisted state changed captured source bytes")
				}
			}
			if mutation == "truncate" || mutation == "rewrite" {
				if len(got.Gaps) == 0 || got.Gaps[0].Code != "continuity_gap" {
					t.Fatal("rewrite or truncation lost continuity evidence")
				}
			}
		})
	}
}

func TestPrefixCheckpointRestartAvoidsInitialPrefixRead(t *testing.T) {
	r, dir, _ := prefixFixture(t)
	data := []byte("{\"type\":\"user\",\"uuid\":\"large\",\"sessionId\":\"synthetic-prefix\",\"message\":{\"role\":\"user\",\"content\":\"" + strings.Repeat("x", 256<<10) + "\"}}\n")
	if err := os.WriteFile(filepath.Join(r.Root, r.Path), data, 0600); err != nil {
		t.Fatal(err)
	}
	first := prefixCapture(t, r, sessioncapture.Checkpoint{}, dir)
	warm, cold := observability.New(), observability.New()
	if _, err := captureFileWithPrefixState(context.Background(), r, first.Checkpoint, warm, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := captureFileMeasured(context.Background(), r, first.Checkpoint, cold); err != nil {
		t.Fatal(err)
	}
	warmBytes := warm.Snapshot().Counters["capture_input_bytes"]
	coldBytes := cold.Snapshot().Counters["capture_input_bytes"]
	if warmBytes >= coldBytes || warmBytes < int64(len(data)) {
		t.Fatalf("expected initial-prefix saving while retaining full final verification: warm=%d cold=%d prefix=%d", warmBytes, coldBytes, len(data))
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("cache directory must be private", err)
	}
	info, err = os.Stat(filepath.Join(dir, prefixCheckpointName(r.Source)))
	if err != nil || info.Mode().Perm() != 0600 || info.Size() > prefixCheckpointLimit {
		t.Fatal("cache file must be private and bounded", err)
	}
}

func TestPrefixCheckpointRejectsInvalidCache(t *testing.T) {
	for _, invalid := range []string{"malformed", "oversized", "wrong_source", "wrong_offset", "wrong_digest", "public", "symlink"} {
		t.Run(invalid, func(t *testing.T) {
			r, dir, _ := prefixFixture(t)
			first := prefixCapture(t, r, sessioncapture.Checkpoint{}, dir)
			file := filepath.Join(dir, prefixCheckpointName(r.Source))
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var cached filePrefixCheckpoint
			if err := json.Unmarshal(data, &cached); err != nil {
				t.Fatal(err)
			}
			switch invalid {
			case "malformed":
				data = []byte("{")
			case "oversized":
				data = bytes.Repeat([]byte(" "), prefixCheckpointLimit+1)
			case "wrong_source":
				cached.State.SourceKey = "wrong"
			case "wrong_offset":
				cached.State.Offset++
			case "wrong_digest":
				cached.State.PrefixDigest = "wrong"
			}
			if strings.HasPrefix(invalid, "wrong_") {
				data, err = json.Marshal(cached)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			if invalid == "public" {
				if err := os.Chmod(file, 0644); err != nil {
					t.Fatal(err)
				}
			}
			var target string
			if invalid == "symlink" {
				target = filepath.Join(t.TempDir(), "target.json")
				if err := os.Rename(file, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, file); err != nil {
					t.Fatal(err)
				}
			}
			if prefixLoad(t, r, first.Checkpoint, dir).Version != 0 {
				t.Fatal("invalid cache was accepted")
			}
			got := prefixCapture(t, r, first.Checkpoint, dir)
			if got.Checkpoint != first.Checkpoint || len(got.Records) != 0 {
				t.Fatal("cache failure changed capture")
			}
			if target != "" {
				unchanged, err := os.ReadFile(target)
				if err != nil || !bytes.Equal(data, unchanged) {
					t.Fatal("symlink target was modified", err)
				}
			}
		})
	}
}

func TestPrefixCheckpointDirectorySymlinkCannotEscape(t *testing.T) {
	r, dir, _ := prefixFixture(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := openPrefixDirectory(dir); err == nil {
		t.Fatal("symlink cache directory accepted")
	}
	got := prefixCapture(t, r, sessioncapture.Checkpoint{}, dir)
	if len(got.Records) != 2 {
		t.Fatal("cache directory failure prevented capture")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("cache escaped through directory symlink", err)
	}
}

func TestPrefixCheckpointCanceledReadDoesNotAdvanceCache(t *testing.T) {
	r, dir, data := prefixFixture(t)
	first := prefixCapture(t, r, sessioncapture.Checkpoint{}, dir)
	file := filepath.Join(dir, prefixCheckpointName(r.Source))
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Root, r.Path), append(data, '{'), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := captureFileWithPrefixState(ctx, r, first.Checkpoint, nil, dir); err == nil {
		t.Fatal("canceled capture succeeded")
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed read advanced cache", err)
	}
}
