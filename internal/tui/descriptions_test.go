package tui

import (
	"github.com/algorhythmic/skald/internal/archive"
	"strings"
	"testing"
)

func TestArchiveDescriptionIgnoresTranscriptWindow(t *testing.T) {
	m := newModel()
	m.width = 180
	m.page = fixturePage("window text should not decide")
	m.descriptionSelection = &archive.DescriptionSelection{Kind: "native_recap", Selected: &archive.TranscriptEntry{Artifact: archive.Artifact{Key: "exact-recap", Revision: "sha256:exact", Adapter: "adapter"}, Text: "Recap outside the recent page", Coverage: "unknown"}, NewActivity: true}
	m.description(0)
	text := linesText(m.lines)
	if !strings.Contains(text, "Recap outside the recent page") || !strings.Contains(text, "archive-wide") || !strings.Contains(text, "exact-recap") || strings.Contains(text, "window text should not decide") {
		t.Fatal(text)
	}
	m.descriptionSelection.OrderingAmbiguous = true
	m.descriptionSelection.PriorRecaps = []archive.TranscriptEntry{{Text: "retained recap"}}
	m.lines = nil
	m.description(0)
	text = linesText(m.lines)
	if !strings.Contains(text, "ordering ambiguous") || strings.Contains(text, "prior native recap") || strings.Contains(text, "newer activity") {
		t.Fatal(text)
	}
	m.clearPrivate()
	if m.descriptionSelection != nil {
		t.Fatal("private description retained")
	}
}
