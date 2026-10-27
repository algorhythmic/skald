package retrieval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/sessioncapture"
)

func TestRealPinnedBraidScopedPublication(t *testing.T) {
	executable := os.Getenv("SKALD_TEST_BRAID")
	if executable == "" {
		t.Skip("set SKALD_TEST_BRAID for real local protocol smoke")
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(binary)
	opts := archive.DefaultOptions()
	opts.MinFreeBytes = 0
	store, err := archive.Open(filepath.Join(t.TempDir(), "archive"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	r := archive.Registration{Source: sessioncapture.Source{Namespace: "synthetic:retrieval", Provider: sessioncapture.Devin, StreamID: "one", ConversationID: "synthetic-conversation"}, Root: t.TempDir(), Path: "synthetic.db"}
	if err := store.Register(ctx, []archive.Registration{r}); err != nil {
		t.Fatal(err)
	}
	raw := []byte("{\"kind\":\"meta\",\"payload\":{}}\n{\"kind\":\"user_message\",\"position\":0,\"payload\":{\"content\":[{\"content\":{\"text\":\"Synthetic nebular quokka research\"}}]}}\n")
	batch, err := sessioncapture.Read(bytes.NewReader(raw), r.Source, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ingest(ctx, r, sessioncapture.Checkpoint{}, batch, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.SetReadScope(ctx, []string{r.Namespace}); err != nil {
		t.Fatal(err)
	}
	generation, _ := store.ReadScopeGeneration(ctx)
	cache := filepath.Join(t.TempDir(), "private-cache")
	config := Config{Executable: executable, SHA256: hex.EncodeToString(sum[:]), Directory: cache, TimeoutMS: 10000}
	engine, err := New(config, store)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	result, err := engine.Query(ctx, []string{r.Namespace}, generation, Request{Text: "quokka", MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || len(result.Context) != 1 || !strings.Contains(result.Context[0].Text, "quokka") {
		t.Fatalf("unexpected retrieval status %s items %d", result.Status, len(result.Context))
	}
	if result.ContextBytes > 4096 || result.Dataset.ID == "default" || len(engine.entries) != 1 {
		t.Fatal("invalid scoped bounded response")
	}
	previous := engine.entries[result.Dataset.ID]
	again, err := engine.Query(ctx, []string{r.Namespace}, generation, Request{Text: "quokka", MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if again.Dataset != result.Dataset || engine.entries[result.Dataset.ID] != previous {
		t.Fatal("unchanged corpus was republished")
	}
	tiny, err := engine.Query(ctx, []string{r.Namespace}, generation, Request{Text: "quokka", MaxBytes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(tiny.Context) != 0 || tiny.ContextBytes > 2 {
		t.Fatal("byte bound exceeded")
	}
	oldDir := engine.entries[result.Dataset.ID].directory
	if err := engine.WithInvalidation(func() error { _, err := store.Redact(ctx, r.Namespace, "source", r.Namespace); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("API redaction retained old published database")
	}
	withdrawn, err := engine.Query(ctx, []string{r.Namespace}, generation, Request{Text: "quokka", MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if withdrawn.Status != "no_answer" || len(withdrawn.Context) != 0 {
		t.Fatal("withdrawn text returned")
	}
	config.Directory = filepath.Join(t.TempDir(), "bad-pin-cache")
	config.SHA256 = strings.Repeat("0", 64)
	bad, err := New(config, store)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if _, err := bad.Query(ctx, []string{r.Namespace}, generation, Request{Text: "quokka"}); err == nil || !strings.Contains(err.Error(), "digest_mismatch") {
		t.Fatalf("pin not enforced: %v", err)
	}
}

func TestRealPinnedBraidMultiFramePublication(t *testing.T) {
	executable := os.Getenv("SKALD_TEST_BRAID")
	if executable == "" {
		t.Skip("set SKALD_TEST_BRAID for real local protocol smoke")
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(binary)
	opts := archive.DefaultOptions()
	opts.MinFreeBytes = 0
	store, err := archive.Open(filepath.Join(t.TempDir(), "archive"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := New(Config{Executable: executable, SHA256: hex.EncodeToString(sum[:]), Directory: filepath.Join(t.TempDir(), "cache"), TimeoutMS: 60000}, store)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	records := make([]archive.RetrievalRecord, 20)
	text := strings.Repeat("x", 1<<20)
	for i := range records {
		records[i] = archive.RetrievalRecord{ID: fmt.Sprintf("r:%d", i), Text: text, Timestamp: "2026-10-22T00:00:00Z", Role: "user"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	publication, err := engine.publish(ctx, "synthetic:multiframe", "synthetic", 1, records)
	if err != nil {
		t.Fatal(err)
	}
	defer publication.client.close()
	defer os.RemoveAll(publication.directory)
	if publication.dataset.Revision < 2 {
		t.Fatal("20 MiB corpus was not published in separate revisions")
	}
	var exact struct {
		Dataset Dataset `json:"dataset"`
		Nodes   []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"nodes"`
	}
	if err := publication.client.call(ctx, "get_many", map[string]any{"get": map[string]any{"dataset_id": "synthetic:multiframe", "expected_revision": publication.dataset.Revision, "ids": []string{"r:19"}}}, &exact); err != nil {
		t.Fatal(err)
	}
	if len(exact.Nodes) != 1 || exact.Nodes[0].Text != text {
		t.Fatal("last frame content missing")
	}
}

func TestWatchdogAndProtocolFault(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("verified executable descriptor uses Linux procfs")
	}
	for _, mode := range []string{"timeout", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "provider")
			body := "#!/bin/sh\nexec sleep 5\n"
			if mode == "malformed" {
				body = "#!/bin/sh\nprintf 'broken JSON\\n'\nexec sleep 5\n"
			}
			if err := os.WriteFile(path, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(body))
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			client, err := start(ctx, Config{Executable: path, SHA256: hex.EncodeToString(sum[:])}, "ignored")
			if err != nil {
				t.Fatal(err)
			}
			defer client.close()
			var result any
			started := time.Now()
			if err := client.call(ctx, "hello", map[string]any{"versions": []int{1}}, &result); err == nil {
				t.Fatal("invalid subprocess accepted")
			}
			if time.Since(started) > time.Second {
				t.Fatal("watchdog did not bound subprocess")
			}
		})
	}
}
