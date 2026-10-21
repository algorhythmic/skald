package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
)

func TestStableBlockedAndPartialTailsAvoidRepeatedCapture(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		name := "partial"
		if blocked {
			name = "blocked"
		}
		t.Run(name, func(t *testing.T) {
			c, file := testCollector(t)
			ctx := context.Background()
			c.Scan(ctx)
			prefix, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			tail := "{"
			if blocked {
				tail = strings.Repeat("x", sessioncapture.DefaultLimits().RecordBytes+1) + "\n"
			}
			if err := os.WriteFile(file, append(append([]byte{}, prefix...), tail...), 0600); err != nil {
				t.Fatal(err)
			}
			c.Scan(ctx)
			key := c.Config.Sources[0].Key()
			cached, ok := c.verified[key]
			if !ok {
				t.Fatal("stable stalled tail was not cached")
			}
			wantHealth, wantIssue := "partial_line", ""
			if blocked {
				wantHealth, wantIssue = "blocked", "capture_blocked"
			}
			for i := 0; i < 3; i++ {
				c.Scan(ctx)
			}
			if c.verified[key].At != cached.At || c.Issues()[key] != wantIssue {
				t.Fatal("unchanged stalled tail was retried or its issue hidden", c.Issues())
			}
			health, err := c.Store.Health(ctx, c.Config.Namespaces())
			if err != nil || health[0].Health != wantHealth {
				t.Fatal("stalled health lost", health, err)
			}
			cached.At = time.Now().Add(-2 * time.Minute)
			c.verified[key] = cached
			c.Scan(ctx)
			if c.verified[key].At == cached.At {
				t.Fatal("stalled prefix audit was skipped")
			}
			fixed := `{"type":"assistant","sessionId":"synthetic-claude","uuid":"fixed","message":{"role":"assistant","content":"recovered"}}` + "\n"
			if err := os.WriteFile(file, append(prefix, fixed...), 0600); err != nil {
				t.Fatal(err)
			}
			c.Scan(ctx)
			stats, err := c.Store.Stats(ctx, c.Config.Namespaces())
			if err != nil || stats["record_versions"] != int64(9) || len(c.Issues()) != 0 {
				t.Fatal("changed stalled source did not recover", stats, c.Issues(), err)
			}
		})
	}
}
