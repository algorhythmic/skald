package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/gdamore/tcell/v2"
)

type row struct {
	session archive.Session
	group   string
}

func (r row) id() string { return r.group + "\x00" + r.session.Key }

type line struct {
	text  string
	tone  string
	row   int
	entry int
	mark  string
}
type model struct {
	palette                                                         palette
	reflow                                                          bool
	diagnostics                                                     bool
	overlayScroll                                                   int
	pendingRecap                                                    bool
	sessionTarget, transcriptTarget                                 int
	snapshot                                                        Snapshot
	rows                                                            []row
	selected                                                        int
	grouping                                                        int
	filter, find, input, previous                                   string
	editing, help, inspecting, expanded, transcript, history, tools bool
	connected                                                       bool
	checked                                                         time.Time
	notice                                                          string
	sessionCursors                                                  []string
	sessionPage                                                     int
	page                                                            archive.Page[archive.TranscriptEntry]
	transcriptCursors                                               []string
	transcriptPage                                                  int
	detailKey                                                       string
	loading                                                         bool
	scroll, overviewScroll                                          int
	lines                                                           []line
	width, height                                                   int
}

func newModel() *model {
	return &model{palette: paletteFor(ThemeDesktop), expanded: true, sessionCursors: []string{""}, transcriptCursors: []string{""}, notice: "Connecting to archive…"}
}

var groupNames = []string{"project", "provider", "source", "all"}

func (m *model) current() *archive.Session {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return nil
	}
	return &m.rows[m.selected].session
}
func (m *model) rebuildRows() {
	id := ""
	if m.selected >= 0 && m.selected < len(m.rows) {
		id = m.rows[m.selected].id()
	}
	m.rows = nil
	for _, s := range m.snapshot.Sessions.Items {
		corpus := s.Title + " " + s.NativeID + " " + s.Provider + " " + s.Namespace
		for _, p := range s.Projects {
			corpus += " " + p.Path
		}
		if !strings.Contains(strings.ToLower(single(corpus)), strings.ToLower(m.filter)) {
			continue
		}
		groups := []string{"all conversations"}
		switch m.grouping {
		case 0:
			groups = nil
			for _, p := range s.Projects {
				groups = append(groups, p.Path)
			}
			if len(groups) == 0 {
				groups = []string{"no project"}
			}
		case 1:
			groups = []string{s.Provider}
		case 2:
			groups = []string{s.Namespace}
		}
		for _, g := range groups {
			m.rows = append(m.rows, row{s, g})
		}
	}
	sort.SliceStable(m.rows, func(i, j int) bool {
		a, b := m.rows[i], m.rows[j]
		if a.group != b.group {
			return a.group < b.group
		}
		if a.session.Title != b.session.Title {
			return a.session.Title < b.session.Title
		}
		return a.session.Key < b.session.Key
	})
	m.selected = min(max(0, m.selected), max(0, len(m.rows)-1))
	for i, r := range m.rows {
		if r.id() == id {
			m.selected = i
			break
		}
	}
}
func (m *model) clearPrivate() {
	m.sessionCursors = []string{""}
	m.sessionPage = 0
	m.sessionTarget = 0
	m.transcriptCursors = []string{""}
	m.transcriptPage = 0
	m.transcriptTarget = 0
	m.loading = false
	m.snapshot = Snapshot{}
	m.rows = nil
	m.page = archive.Page[archive.TranscriptEntry]{}
	m.detailKey = ""
	m.lines = nil
	m.transcript = false
	m.expanded = false
	m.inspecting = false
	m.scroll = 0
	m.overviewScroll = 0
}
func (m *model) selectedEntry() *archive.TranscriptEntry {
	if len(m.lines) == 0 {
		return nil
	}
	for i := min(m.scroll, len(m.lines)-1); i < len(m.lines); i++ {
		n := m.lines[i].entry
		if n >= 0 && n < len(m.page.Items) {
			return &m.page.Items[n]
		}
	}
	return nil
}
func (m *model) copyReference(screen tcell.Screen) {
	e := m.selectedEntry()
	if e == nil {
		m.notice = "No record at this position"
		return
	}
	b, _ := json.MarshalIndent(map[string]string{"record_key": e.Key, "source_revision": e.Revision, "adapter_version": e.Adapter}, "", "  ")
	screen.SetClipboard(b)
	m.notice = "Reference sent to terminal clipboard (terminal support required)"
}
func (m *model) jump(mark string, direction int) {
	if len(m.lines) == 0 {
		return
	}
	for i := m.scroll + direction; i >= 0 && i < len(m.lines); i += direction {
		if m.lines[i].mark == mark {
			m.scroll = i
			return
		}
	}
	m.notice = "No more " + mark + " markers on this page · N older / P newer"
}
func (m *model) findNext(direction int) {
	if m.find == "" || len(m.lines) == 0 {
		return
	}
	for step := 1; step <= len(m.lines); step++ {
		i := (m.scroll + step*direction + len(m.lines)) % len(m.lines)
		if strings.Contains(strings.ToLower(m.lines[i].text), strings.ToLower(m.find)) {
			m.scroll = i
			m.notice = "Find on this page · f next / F previous"
			return
		}
	}
	m.notice = "No match on this page (unfold tools to search their text)"
}
func (m *model) resetDetail() {
	m.page = archive.Page[archive.TranscriptEntry]{}
	m.transcriptCursors = []string{""}
	m.transcriptPage = 0
	m.transcriptTarget = 0
	m.detailKey = ""
	m.scroll = 0
	m.inspecting = false
}
func (m *model) move(delta int) {
	m.selected = min(max(0, m.selected+delta), max(0, len(m.rows)-1))
	m.resetDetail()
}
func sourceTime(e archive.TranscriptEntry) string {
	if e.SourceTime == nil {
		return "time unknown"
	}
	t, err := time.Parse(time.RFC3339Nano, *e.SourceTime)
	if err != nil {
		return single(*e.SourceTime)
	}
	return t.Local().Format("02 Jan 15:04")
}
func role(e archive.TranscriptEntry) string {
	if e.Role == "user" {
		return "you"
	}
	if e.Role != "" {
		return e.Role
	}
	return strings.ReplaceAll(e.Kind, "_", " ")
}
func position(e archive.TranscriptEntry) string {
	return fmt.Sprintf("stream %s · epoch %d · #%d", short(e.Stream), e.Epoch, e.Ordinal)
}
func short(s string) string {
	if len(s) > 18 {
		return s[:8] + "…" + s[len(s)-8:]
	}
	return s
}
