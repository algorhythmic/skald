package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"
	"skald/internal/archive"
)

func TestUnicodeControlsAndNarrowScreens(t *testing.T) {
	text := "hello\x1b]52;c;attack\a\r\u202e界 e\u0301 👩‍💻\nnext"
	escaped := safeText(text)
	if strings.ContainsAny(escaped, "\x1b\a\r\u202e") || !strings.Contains(escaped, "\\u001b") || !strings.Contains(escaped, "👩‍💻") {
		t.Fatal("unsafe display", escaped)
	}
	for _, w := range []int{1, 2, 10, 32, 80} {
		for _, line := range wrap(text, w) {
			if !utf8.ValidString(line) || uniseg.StringWidth(line) > w {
				t.Fatalf("width %d: %q", w, line)
			}
		}
	}
	m := newModel()
	m.connected = true
	m.snapshot = fixtureSnapshot()
	m.rebuildRows()
	m.page = fixturePage("answer")
	m.detailKey = "a"
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	for _, size := range [][2]int{{0, 0}, {10, 3}, {32, 10}, {60, 18}, {120, 40}} {
		s.SetSize(size[0], size[1])
		m.draw(s)
		m.transcript = true
		m.draw(s)
		m.help = true
		m.draw(s)
		m.help = false
		m.transcript = false
	}
}
func TestProjectGroupingAndRecapSelection(t *testing.T) {
	m := newModel()
	m.snapshot = fixtureSnapshot()
	m.snapshot.Sessions.Items[0].Projects = append(m.snapshot.Sessions.Items[0].Projects, archive.Project{Path: "/second"})
	m.rebuildRows()
	if len(m.rows) != 3 {
		t.Fatal("multiple project associations lost")
	}
	m.width = 120
	m.page = fixturePage("Newer assistant answer")
	m.description(0)
	text := linesText(m.lines)
	if !strings.Contains(text, "assistant excerpt") || !strings.Contains(text, "prior native recap") || !strings.Contains(text, "coverage unknown") {
		t.Fatal(text)
	}
	// A later user/tool record keeps the recap; it cannot become an assistant excerpt.
	m.page.Items[0].Role = "user"
	m.lines = nil
	m.description(0)
	text = linesText(m.lines)
	if strings.Contains(text, "assistant excerpt") || !strings.Contains(text, "newer activity") {
		t.Fatal(text)
	}
	m.page.Items[0].Role = "assistant"
	m.page.Items[0].Stream = "other"
	m.lines = nil
	m.description(0)
	text = linesText(m.lines)
	if !strings.Contains(text, "ordering ambiguous") || strings.Contains(text, "prior native recap") || strings.Contains(text, "newer activity") {
		t.Fatal("cross-stream chronology invented", text)
	}
	m.filter = "not a match"
	m.rebuildRows()
	if len(m.rows) != 0 {
		t.Fatal("filter ignored")
	}
	m.clearPrivate()
	if len(m.page.Items) > 0 || len(m.rows) > 0 || len(m.lines) > 0 {
		t.Fatal("private display not cleared")
	}
}
func linesText(lines []line) string {
	var out strings.Builder
	for _, l := range lines {
		out.WriteString(l.text)
		out.WriteByte('\n')
	}
	return out.String()
}
func fixtureSnapshot() Snapshot {
	return Snapshot{Status: Status{Instance: "fixture", Boundary: 1, Sessions: 2}, Sessions: archive.Page[archive.Session]{Boundary: 1, Items: []archive.Session{
		{Key: "a", Title: "Alpha conversation", Namespace: "fixture:a", Provider: "claude_code", Activity: "unknown", SourceHealth: "available", Projects: []archive.Project{{Path: "/fixtures/skald"}}},
		{Key: "b", Title: "Beta conversation", Namespace: "fixture:b", Provider: "codex", Activity: "unknown", SourceHealth: "unavailable"},
	}}}
}
func fixturePage(text string) archive.Page[archive.TranscriptEntry] {
	return archive.Page[archive.TranscriptEntry]{Boundary: 1, Items: []archive.TranscriptEntry{
		{Artifact: archive.Artifact{Key: "answer", Kind: "message", Current: true, Stream: "s", Ordinal: 3}, Role: "assistant", Text: text, Coverage: "unknown"},
		{Artifact: archive.Artifact{Key: "tool", Kind: "tool_call", Current: true, Stream: "s", Ordinal: 2}, Parts: []archive.DisplayPart{{Type: "tool_call", Name: "Read", Text: "tool detail value"}}},
		{Artifact: archive.Artifact{Key: "recap", Kind: "native_recap", Current: true, Stream: "s", Ordinal: 1}, Text: "Native recap content", Coverage: "unknown"},
		{Artifact: archive.Artifact{Key: "user", Kind: "message", Current: true, Stream: "s", Ordinal: 0}, Role: "user", Text: "The user request"},
	}}
}

type snapshotReply struct {
	value Snapshot
	err   error
}
type pageReply struct {
	value archive.Page[archive.TranscriptEntry]
	err   error
}
type snapshotRequest struct {
	cursor string
	reply  chan snapshotReply
}
type pageRequest struct {
	key, cursor string
	history     bool
	reply       chan pageReply
}
type fakeBackend struct {
	snap chan snapshotRequest
	page chan pageRequest
}

func (f *fakeBackend) Snapshot(ctx context.Context, cursor string) (Snapshot, error) {
	r := snapshotRequest{cursor, make(chan snapshotReply, 1)}
	select {
	case f.snap <- r:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
	select {
	case v := <-r.reply:
		return v.value, v.err
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
}
func (f *fakeBackend) Transcript(ctx context.Context, key, cursor string, history bool) (archive.Page[archive.TranscriptEntry], error) {
	r := pageRequest{key, cursor, history, make(chan pageReply, 1)}
	select {
	case f.page <- r:
	case <-ctx.Done():
		return archive.Page[archive.TranscriptEntry]{}, ctx.Err()
	}
	// Intentionally ignore cancellation until the test replies: stale responses
	// must be rejected by request identity even when a backend is slow to cancel.
	v := <-r.reply
	return v.value, v.err
}

type observedScreen struct {
	tcell.SimulationScreen
	views chan string
}

func (s *observedScreen) Show() {
	s.SimulationScreen.Show()
	cells, w, h := s.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			b.WriteString(string(cells[y*w+x].Runes))
		}
		b.WriteByte('\n')
	}
	select {
	case s.views <- b.String():
	default:
	}
}
func waitFor[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("timed out")
		var zero T
		return zero
	}
}
func waitView(t *testing.T, s *observedScreen, want string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	last := ""
	for {
		select {
		case last = <-s.views:
			if strings.Contains(last, want) {
				return last
			}
		case <-deadline:
			t.Fatalf("view missing %q:\n%s", want, last)
			return ""
		}
	}
}
func key(s *observedScreen, r rune) { s.InjectKey(tcell.KeyRune, r, 0) }
func TestEventLoopSelectionStaleResultsTranscriptReconnectAndDeniedScope(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	defer sim.Fini()
	sim.SetSize(120, 40)
	screen := &observedScreen{sim, make(chan string, 100)}
	backend := &fakeBackend{make(chan snapshotRequest, 10), make(chan pageRequest, 10)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runScreen(ctx, screen, backend, ThemeDesktop) }()
	snap := waitFor(t, backend.snap)
	snap.reply <- snapshotReply{value: fixtureSnapshot()}
	first := waitFor(t, backend.page)
	if first.key != "a" {
		t.Fatal(first.key)
	}
	key(screen, 'j')
	second := waitFor(t, backend.page)
	if second.key != "b" {
		t.Fatal(second.key)
	}
	second.reply <- pageReply{value: fixturePage("Selected Beta answer")}
	waitView(t, screen, "Selected Beta answer")
	first.reply <- pageReply{value: fixturePage("STALE ALPHA CONTENT")}
	screen.InjectKey(tcell.KeyEnter, 0, 0)
	v := waitView(t, screen, "transcript / Beta conversation")
	if strings.Contains(v, "STALE ALPHA CONTENT") {
		t.Fatal("stale detail replaced selection")
	}
	key(screen, ' ')
	waitView(t, screen, "tool detail value")
	key(screen, '/')
	for _, r := range "Beta" {
		key(screen, r)
	}
	screen.InjectKey(tcell.KeyEnter, 0, 0)
	waitView(t, screen, "Find on this page")
	key(screen, 'i')
	waitView(t, screen, "record reference / original retained")
	screen.InjectKey(tcell.KeyEscape, 0, 0)
	key(screen, 'r')
	snapshot := waitFor(t, backend.snap)
	page := waitFor(t, backend.page)
	snapshot.reply <- snapshotReply{err: errors.New("daemon_unavailable")}
	page.reply <- pageReply{err: errors.New("daemon_unavailable")}
	waitView(t, screen, "OFFLINE")
	key(screen, 'r')
	snapshot = waitFor(t, backend.snap)
	page = waitFor(t, backend.page)
	page.reply <- pageReply{value: fixturePage("Reconnected Beta answer")}
	snapshot.reply <- snapshotReply{value: fixtureSnapshot()}
	reconnect := waitFor(t, backend.page)
	reconnect.reply <- pageReply{value: fixturePage("Reconnected Beta answer")}
	waitView(t, screen, "Reconnected Beta answer")
	key(screen, 'r')
	snapshot = waitFor(t, backend.snap)
	page = waitFor(t, backend.page)
	snapshot.reply <- snapshotReply{err: errors.New("scope_denied")}
	waitView(t, screen, "Access denied")
	page.reply <- pageReply{value: fixturePage("PRIVATE LATE REPLY")}
	key(screen, '?')
	v = waitView(t, screen, "skald / keyboard")
	if strings.Contains(v, "PRIVATE LATE REPLY") {
		t.Fatal("revoked content reappeared")
	}
	key(screen, '?')
	key(screen, 'q')
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
}

type monochromeScreen struct{ tcell.Screen }

func (s monochromeScreen) Colors() int { return 0 }
func TestMonochromeUsesAttributesWithoutBrightnessInversion(t *testing.T) {
	screen := monochromeScreen{}
	p := paletteFor(ThemeAmber)
	for _, style := range []tcell.Style{p.base, p.dim, p.accent, p.bright, p.border} {
		fg, bg, attr := displayStyle(screen, style).Decompose()
		if fg != tcell.ColorDefault || bg != tcell.ColorDefault || attr&tcell.AttrReverse != 0 {
			t.Fatal("implicit monochrome inversion")
		}
	}
	_, _, attr := displayStyle(screen, p.base.Background(tcell.NewHexColor(0x282820))).Decompose()
	if attr&tcell.AttrReverse == 0 {
		t.Fatal("selection lacks monochrome emphasis")
	}
}

func TestResizeAndToolFoldingPreserveReadingRecord(t *testing.T) {
	m := newModel()
	m.snapshot = fixtureSnapshot()
	m.rebuildRows()
	m.connected = true
	m.transcript = true
	m.page = fixturePage(strings.Repeat("A long final answer. ", 200))
	m.detailKey = "a"
	m.page.Items[3].Text = strings.Repeat("Earlier user content. ", 200)
	m.page.Items[1].Parts[0].Text = strings.Repeat("tool payload ", 200)
	m.tools = true
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(100, 20)
	m.draw(s)
	for i, l := range m.lines {
		if l.entry == 1 {
			m.scroll = i + 1
			break
		}
	}
	m.draw(s)
	s.SetSize(50, 20)
	m.draw(s)
	if e := m.selectedEntry(); e == nil || e.Key != "tool" {
		t.Fatal("resize moved to a different record", e)
	}
	m.tools = false
	m.reflow = true
	m.draw(s)
	if e := m.selectedEntry(); e == nil || e.Key != "tool" {
		t.Fatal("folding moved to a different record", e)
	}
}
