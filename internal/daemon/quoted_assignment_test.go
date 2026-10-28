package daemon

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestQuotedAssignmentBackfillAfterRestart(t *testing.T) {
	c, file := testCollector(t)
	ctx := context.Background()
	r := c.Config.Sources[0]
	c.Scan(ctx)
	before, err := c.Store.Checkpoint(ctx, r.Key())
	if err != nil {
		t.Fatal("initial checkpoint")
	}
	value := strings.Repeat("Q", 18)
	raw, _ := json.Marshal(map[string]any{"type": "assistant", "sessionId": "synthetic-claude", "uuid": "quoted-tail", "message": map[string]string{"role": "assistant", "content": "password=\"" + value + "\".suffix123"}})
	raw = append(raw, '\n')
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("fixture append")
	}
	_, err = f.Write(raw)
	f.Close()
	if err != nil {
		t.Fatal("fixture append")
	}
	// Model the old build's failed attempt: unavailable, committed prefix retained.
	if c.Store.SetHealth(ctx, r, "unavailable", time.Now()) != nil {
		t.Fatal("fixture health")
	}
	restarted := &Collector{Store: c.Store, Config: c.Config}
	if restarted.Initialize(ctx) != nil {
		t.Fatal("collector restart")
	}
	restarted.Scan(ctx)
	after, err := c.Store.Checkpoint(ctx, r.Key())
	if err != nil || after.Offset-before.Offset != int64(len(raw)) || after.Ordinal != before.Ordinal+1 || after.HasGaps {
		t.Fatal("tail checkpoint did not advance")
	}
	health, err := c.Store.Health(ctx, c.Config.Namespaces())
	if err != nil || len(health) != 1 || health[0].Health != "available" || len(restarted.Issues()) != 0 {
		t.Fatal("capture health did not recover")
	}
	records, err := c.Store.Stats(ctx, c.Config.Namespaces())
	if err != nil || records["record_versions"] != int64(9) {
		t.Fatal("tail not stored")
	}
	restarted.Scan(ctx)
	replay, err := c.Store.Stats(ctx, c.Config.Namespaces())
	if err != nil || replay["record_versions"] != records["record_versions"] {
		t.Fatal("tail duplicated")
	}
}
