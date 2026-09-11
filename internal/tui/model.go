package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/gdamore/tcell/v2"
)

type row struct {
	session archive.Session
	group   string
	// cluster marks a collapsed/expanded group header for sessions that share
	// an identical display title inside one group; nil for ordinary rows.
	cluster []archive.Session
}

func (r row) id() string {
	if r.cluster != nil {
		return r.group + "\x00cluster\x00" + rowTitle(r.cluster[0])
	}
	return r.group + "\x00" + r.session.Key
}

// rowTitle is the display identity used for collapsing: sessions with no
// title evidence share the honest "(untitled)" label so stub sessions fold
// into one expandable row instead of a run of hex identifiers.
func rowTitle(s archive.Session) string {
	if s.TitleKind == "" {
		return "(untitled)"
	}
	return s.Title
}

// foldKey clusters sessions whose titles share a common prefix: generated
// titles often differ only in a trailing URL, port or counter, so folding
// needs the shared head rather than the full string.
func foldTitle(s archive.Session) string {
	t := []rune(rowTitle(s))
	if len(t) > 60 {
		t = t[:60]
	}
	return string(t)
}

func clusterKey(group string, members []archive.Session) string {
	return group + "\x00" + foldTitle(members[0])
}

// clusterTitle is the longest shared head of member titles, so a header
// never shows one member's distinct tail as the cluster's name.
func clusterTitle(members []archive.Session) string {
	t := rowTitle(members[0])
	for _, s := range members[1:] {
		o := rowTitle(s)
		for t != "" && !strings.HasPrefix(o, t) {
			r := []rune(t)
			t = string(r[:len(r)-1])
		}
	}
	return t
}

type line struct {
	text  string
	tone  string
	row   int
	entry int
	mark  string
	dot   string
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
	ordering                                                        int
	filter, find, input, previous                                   string
	editing, help, inspecting, expanded, transcript, history, tools bool
	connected                                                       bool
	hideIdle                                                        bool
	expandedClusters                                                map[string]bool
	checked                                                         time.Time
	notice                                                          string
	sessionCursors                                                  []string
	sessionPage                                                     int
	page                                                            archive.Page[archive.TranscriptEntry]
	transcriptCursors                                               []string
	transcriptPage                                                  int
	detailKey                                                       string
	pageKey                                                         string
	loading                                                         bool
	scroll, overviewScroll                                          int
	lines                                                           []line
	width, height                                                   int
}

func newModel() *model {
	return &model{palette: paletteFor(ThemeHeimdall), expanded: true, sessionCursors: []string{""}, transcriptCursors: []string{""}, notice: "Connecting to archive…"}
}

var groupNames = []string{"project", "provider", "source", "recency", "status", "all"}
var orderNames = []string{"recent", "title", "status"}

// groupOrder gives recency and status buckets a meaningful order instead of
// alphabetical; unlisted groups sort last.
var groupOrder = map[string][]string{
	"recency": {"last hour", "last day", "last week", "older", "no observed activity"},
	"status":  {"input requested", "source problem", "working", "idle", "unknown"},
}

func groupRank(order []string, group string) int {
	for i, o := range order {
		if o == group {
			return i
		}
	}
	return len(order)
}

func recentlyActive(ts *string) bool {
	switch recencyBucket(ts) {
	case "last hour", "last day":
		return true
	}
	return false
}

func recencyBucket(ts *string) string {
	if ts == nil {
		return "no observed activity"
	}
	t, err := time.Parse(time.RFC3339Nano, *ts)
	if err != nil {
		return "no observed activity"
	}
	switch d := time.Since(t); {
	case d < time.Hour:
		return "last hour"
	case d < 24*time.Hour:
		return "last day"
	case d < 7*24*time.Hour:
		return "last week"
	}
	return "older"
}

// statusBucket mirrors the activity dot: a source problem outranks activity,
// then input requested, working, idle and unknown.
func statusBucket(s archive.Session) string {
	if s.SourceHealth == "blocked" {
		return "source problem"
	}
	switch s.Activity {
	case "input":
		return "input requested"
	case "working":
		return "working"
	case "idle":
		return "idle"
	}
	return "unknown"
}

// unitRank is a unit's most urgent member status under the status bucket
// order; clusters rank by their most urgent member.
func unitRank(head row) int {
	rank := len(groupOrder["status"])
	members := head.cluster
	if members == nil {
		members = []archive.Session{head.session}
	}
	for _, s := range members {
		if r := groupRank(groupOrder["status"], statusBucket(s)); r < rank {
			rank = r
		}
	}
	return rank
}

// sessionLess orders sessions inside an expanded cluster the same way the
// unit sort orders rows: status ranks urgent evidence first, title sorts
// alphabetically, recent puts the newest record first.
func (m *model) sessionLess(a, b archive.Session) bool {
	if orderNames[m.ordering] == "status" {
		if ra, rb := groupRank(groupOrder["status"], statusBucket(a)), groupRank(groupOrder["status"], statusBucket(b)); ra != rb {
			return ra < rb
		}
	}
	if orderNames[m.ordering] != "title" {
		if (a.LastRecordTime == nil) != (b.LastRecordTime == nil) {
			return b.LastRecordTime == nil
		}
		if a.LastRecordTime != nil && *a.LastRecordTime != *b.LastRecordTime {
			return *a.LastRecordTime > *b.LastRecordTime
		}
	}
	if a.Title != b.Title {
		return a.Title < b.Title
	}
	return a.Key < b.Key
}

func (m *model) current() *archive.Session {
	if m.selected < 0 || m.selected >= len(m.rows) || m.rows[m.selected].cluster != nil {
		return nil
	}
	return &m.rows[m.selected].session
}

// sourceLabel renders a grouping name for a namespace from the configured
// source root; unknown namespaces keep a short token rather than the full key.
func (m *model) sourceLabel(ns string) string {
	for _, d := range m.snapshot.Status.Discovery {
		if d.Namespace == ns {
			if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(d.Root, home+"/") {
				return "~/" + strings.TrimPrefix(d.Root, home+"/")
			}
			return d.Root
		}
	}
	return shortID(ns)
}
func (m *model) rebuildRows() {
	id := ""
	if m.selected >= 0 && m.selected < len(m.rows) {
		id = m.rows[m.selected].id()
	}
	m.rows = nil
	for _, s := range m.snapshot.Sessions.Items {
		if m.hideIdle && s.Activity == "idle" {
			continue
		}
		corpus := s.Title + " " + s.NativeID + " " + s.Provider + " " + s.Namespace
		for _, p := range s.Projects {
			corpus += " " + p.Path
		}
		if !strings.Contains(strings.ToLower(single(corpus)), strings.ToLower(m.filter)) {
			continue
		}
		groups := []string{"all conversations"}
		switch groupNames[m.grouping] {
		case "project":
			groups = nil
			for _, p := range s.Projects {
				groups = append(groups, p.Path)
			}
			if len(groups) == 0 {
				groups = []string{"no project"}
			}
		case "provider":
			groups = []string{s.Provider}
		case "source":
			groups = []string{m.sourceLabel(s.Namespace)}
		case "recency":
			groups = []string{recencyBucket(s.LastRecordTime)}
		case "status":
			groups = []string{statusBucket(s)}
		}
		for _, g := range groups {
			m.rows = append(m.rows, row{session: s, group: g})
		}
	}
	// Project groups rank by live evidence: the most active or recently
	// active sessions first, then the most broken ones; other groupings keep
	// their bucket order or sort alphabetically.
	stats := map[string][2]int{}
	if groupNames[m.grouping] == "project" {
		for _, r := range m.rows {
			s := r.session
			st := stats[r.group]
			if s.Activity == "working" || s.Activity == "input" || recentlyActive(s.LastRecordTime) {
				st[0]++
			}
			if s.SourceHealth == "blocked" || s.SourceHealth == "capture_gaps" {
				st[1]++
			}
			stats[r.group] = st
		}
	}
	groupLess := func(a, b string) bool {
		if order := groupOrder[groupNames[m.grouping]]; order != nil {
			return groupRank(order, a) < groupRank(order, b)
		}
		if sa, sb := stats[a], stats[b]; sa != sb {
			if sa[0] != sb[0] {
				return sa[0] > sb[0]
			}
			return sa[1] > sb[1]
		}
		return a < b
	}
	// Phase one sorts by title so identical display titles fold together;
	// phase two orders the folded units by newest member activity.
	sort.SliceStable(m.rows, func(i, j int) bool {
		a, b := m.rows[i], m.rows[j]
		if a.group != b.group {
			return groupLess(a.group, b.group)
		}
		if fa, fb := foldTitle(a.session), foldTitle(b.session); fa != fb {
			return fa < fb
		}
		if a.session.Title != b.session.Title {
			return a.session.Title < b.session.Title
		}
		return a.session.Key < b.session.Key
	})
	type unit struct {
		head row
		kids []row
		ts   *string
		fold string
	}
	var units []unit
	for i := 0; i < len(m.rows); {
		j := i + 1
		for j < len(m.rows) && m.rows[j].group == m.rows[i].group && foldTitle(m.rows[j].session) == foldTitle(m.rows[i].session) {
			j++
		}
		members := m.rows[i:j]
		u := unit{fold: foldTitle(members[0].session)}
		for _, r := range members {
			if t := r.session.LastRecordTime; t != nil && (u.ts == nil || *t > *u.ts) {
				u.ts = t
			}
		}
		if len(members) > 1 {
			set := make([]archive.Session, 0, len(members))
			for _, r := range members {
				set = append(set, r.session)
			}
			u.head = row{group: members[0].group, cluster: set}
			sort.SliceStable(members, func(i, j int) bool { return m.sessionLess(members[i].session, members[j].session) })
			if m.expandedClusters[clusterKey(members[0].group, set)] {
				u.kids = members
			}
		} else {
			u.head = members[0]
		}
		units = append(units, u)
		i = j
	}
	sort.SliceStable(units, func(i, j int) bool {
		a, b := units[i], units[j]
		if a.head.group != b.head.group {
			return groupLess(a.head.group, b.head.group)
		}
		if orderNames[m.ordering] == "status" {
			if ra, rb := unitRank(a.head), unitRank(b.head); ra != rb {
				return ra < rb
			}
		}
		if orderNames[m.ordering] != "title" {
			if (a.ts == nil) != (b.ts == nil) {
				return b.ts == nil
			}
			if a.ts != nil && *a.ts != *b.ts {
				return *a.ts > *b.ts
			}
		}
		if a.fold != b.fold {
			return a.fold < b.fold
		}
		return a.head.id() < b.head.id()
	})
	m.rows = nil
	for _, u := range units {
		m.rows = append(m.rows, u.head)
		m.rows = append(m.rows, u.kids...)
	}
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
	m.pageKey = ""
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
