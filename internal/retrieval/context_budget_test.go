package retrieval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/sessioncapture"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/algorhythmic/skald/sessionrecord"
)

func TestFinalCitationBudgetKeepsRankedPrefix(t *testing.T) {
	first := Item{ID: "r:1", Text: strings.Repeat("Synthetic quoted \"evidence\".\n", 8), Ref: sessionrecord.Ref{RecordKey: strings.Repeat("k", 71), SourceRevision: "sha256:" + strings.Repeat("a", 64)}, Adapter: "synthetic-adapter", Namespace: strings.Repeat("synthetic:namespace:", 10), Role: "assistant"}
	second := first
	second.ID = "r:2"
	second.Ref.RecordKey = strings.Repeat("j", 71)
	items := []Item{first, second}
	one, _ := json.Marshal(items[:1])
	all, _ := json.Marshal(items)
	budget := len(one) + 10
	// Both text-only Braid projections fit this budget. Adding exact citations
	// exceeds it; the former all-or-empty policy would discard both results.
	projection, _ := json.Marshal([]map[string]string{{"id": first.ID, "text": first.Text}, {"id": second.ID, "text": second.Text}})
	if len(projection) > budget || len(all) <= budget {
		t.Fatal("regression fixture does not exercise citation overhead")
	}
	kept, n := fitContext(items, budget)
	encoded, _ := json.Marshal(kept)
	if len(kept) != 1 || !reflect.DeepEqual(kept[0], first) || n != len(encoded) || n > budget {
		t.Fatal("complete top-ranked evidence was not retained within final JSON limit")
	}
	exact, n := fitContext(items, len(all))
	if len(exact) != 2 || n != len(all) {
		t.Fatal("exact-fitting citations were dropped")
	}
	empty, n := fitContext(items, len(one)-1)
	if len(empty) != 0 || n != 2 {
		t.Fatal("oversized first item was truncated or replaced with lower-ranked evidence")
	}
}

func TestRealPinnedBraidFinalCitationBudgetKeepsEvidence(t *testing.T) {
	executable := os.Getenv("SKALD_TEST_BRAID")
	if executable == "" {
		t.Skip("set SKALD_TEST_BRAID for real local protocol regression")
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(binary)
	options := archive.DefaultOptions()
	options.MinFreeBytes = 0
	store, err := archive.Open(filepath.Join(t.TempDir(), "archive"), options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	r := archive.Registration{Source: sessioncapture.Source{Namespace: "synthetic:final-budget", Provider: sessioncapture.Devin, StreamID: "budget", ConversationID: "synthetic-budget"}, Root: t.TempDir(), Path: "synthetic.db"}
	if err := store.Register(ctx, []archive.Registration{r}); err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	raw.WriteString("{\"kind\":\"meta\",\"payload\":{}}\n")
	for i := 0; i < 2; i++ {
		line, _ := json.Marshal(map[string]any{"kind": "user_message", "position": i, "payload": map[string]any{"content": []any{map[string]any{"content": map[string]string{"text": "Synthetic quokka " + strings.Repeat("\n", 1024)}}}}})
		raw.Write(line)
		raw.WriteByte('\n')
	}
	batch, err := sessioncapture.ReadSnapshot(raw.Bytes(), r.Source, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), time.Now())
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
	engine, err := New(Config{Executable: executable, SHA256: hex.EncodeToString(sum[:]), Directory: filepath.Join(t.TempDir(), "cache"), TimeoutMS: 10000}, store)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	full, err := engine.Query(ctx, []string{r.Namespace}, generation, Request{Text: "quokka", MaxBytes: 16384})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Context) != 2 {
		t.Fatal("synthetic two-item fixture did not retrieve both")
	}
	budget := full.ContextBytes - 1
	bounded, err := engine.Query(ctx, []string{r.Namespace}, generation, Request{Text: "quokka", MaxBytes: budget})
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(bounded.Context)
	if bounded.Status != "ok" || len(bounded.Context) != 1 || !reflect.DeepEqual(bounded.Context[0], full.Context[0]) || bounded.ContextBytes != len(wire) || len(wire) > budget {
		t.Fatal("citation overhead discarded all evidence or changed ranked top item")
	}
}
