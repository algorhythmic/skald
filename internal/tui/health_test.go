package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
)

func TestDiagnosticsKeepUnknownAgeAndCheckpointVisible(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	captured := now.Add(-2 * time.Hour).Format(time.RFC3339)
	future := now.Add(time.Hour).Format(time.RFC3339)
	generation := "fixture-generation"
	epoch, nextOrdinal := int64(2), int64(7)
	m := newModel()
	m.snapshot.Status = Status{SourcesTotal: 258, SourcesOmitted: 256, Sources: []archive.SourceHealth{
		{Key: "first", Namespace: "fixture", Provider: "codex", Enrolled: true, Health: "unavailable", CheckpointAvailable: true, Generation: &generation, Epoch: &epoch, NextOrdinal: &nextOrdinal, CapturedOffset: 123, ParsedOffset: 120, HasGaps: true, CheckedAt: &future, LastCapturedAt: &captured},
		{Key: "second", Namespace: "fixture", Provider: "claude_code", Enrolled: true, Health: "unknown"},
	}, Issues: map[string]string{"first": "source_unavailable"}}
	text := strings.Join(m.diagnosticLines(now), "\n")
	for _, want := range []string{
		"2 of 258 enrolled streams · 256 omitted", "Enrollment: enrolled", "Checkpoint: epoch 2 · next ordinal 7", "Captured 123 bytes · parsed 120 bytes · continuity gaps: true", "Last captured record: " + captured + " (2h ago)", "Latest record source time: unknown", "Checkpoint: unavailable · capture position unknown", "Source checked: unknown", "age unknown: clock ahead", "Capture issue: source_unavailable", "do not establish live presence or task completion",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("diagnostics missing %q:\n%s", want, text)
		}
	}
}

func TestDiagnosticTimeUnknownAndElapsed(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, raw := range []string{"", "invalid", "0001-01-01T00:00:00Z"} {
		if got := diagnosticTime(&raw, now); got != "unknown" {
			t.Fatalf("invalid age %q became %q", raw, got)
		}
	}
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{0, "<1m ago"}, {3 * time.Minute, "3m ago"}, {2 * time.Hour, "2h ago"}, {48 * time.Hour, "2d ago"},
	} {
		raw := now.Add(-tc.age).Format(time.RFC3339Nano)
		if got := diagnosticTime(&raw, now); !strings.Contains(got, tc.want) {
			t.Fatalf("age %s: %s", tc.age, got)
		}
	}
}
