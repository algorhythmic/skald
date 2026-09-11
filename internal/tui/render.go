package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"
)

func (m *model) add(text, color string, row, entry int, mark string) {
	m.lines = append(m.lines, line{text, color, row, entry, mark})
}
func (m *model) paragraph(text, prefix, color string, row, entry, maxLines int) {
	lines := wrap(text, max(1, min(m.width-8-uniseg.StringWidth(prefix), 108)))
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
		lines[len(lines)-1] = clip(lines[len(lines)-1], max(1, m.width-12)) + " …"
	}
	for _, s := range lines {
		m.add(prefix+s, color, row, entry, "")
	}
}
func (m *model) overview() {
	lastGroup := ""
	for i, r := range m.rows {
		s := r.session
		if r.group != lastGroup {
			if lastGroup != "" {
				m.add("", "", -1, -1, "")
			}
			m.add("▾ "+single(r.group), "amber", -1, -1, "")
			lastGroup = r.group
		}
		marker := "├─ ○ "
		if i == m.selected {
			marker = "▸  ○ "
		}
		m.add(marker+single(s.Title), "bright", i, -1, "")
		meta := "│    " + s.Provider + " · activity " + s.Activity + " · source " + s.SourceHealth + fmt.Sprintf(" · %d versions", s.RecordVersions)
		if s.TitleKind == "derived" {
			meta += " · derived title"
		}
		m.add(single(meta), "dim", i, -1, "")
		if len(s.Projects) > 1 || s.ProjectsTruncated {
			m.add("│    multiple observed projects · association retained with source reference", "dim", i, -1, "")
		}
		if s.TitleOrderingAmbiguous {
			m.add("│    title ordering ambiguous across source streams", "warn", i, -1, "")
		}
		if i == m.selected && m.expanded {
			m.add("│", "dim", i, -1, "")
			if m.loading {
				m.add("│    Loading recent context…", "dim", i, -1, "")
			} else if m.detailKey == s.Key {
				m.description(i)
			}
			m.add("│    Enter transcript  ·  h recaps  ·  surface unavailable", "amber", i, -1, "")
		}
	}
	if len(m.rows) == 0 {
		if !m.connected {
			m.add("Waiting for the archive daemon", "bright", -1, -1, "")
			m.paragraph("Start skald serve with an explicit source configuration. This view reconnects automatically.", "", "dim", -1, -1, 0)
		} else if m.filter != "" {
			m.add("No matches on this session page", "bright", -1, -1, "")
			m.add("/ change filter · N next session page", "dim", -1, -1, "")
		} else if len(m.snapshot.Status.Discovery) > 0 {
			m.add("No conversations enrolled yet", "bright", -1, -1, "")
			m.paragraph("Selected roots are being monitored. Press d to inspect discovery progress, the collection cutoff, and any rejected files.", "", "dim", -1, -1, 0)
		} else {
			m.add("No archived conversations yet", "bright", -1, -1, "")
			m.paragraph("Configure a Claude Code or Codex source, then let the daemon capture it. No personal folders are scanned automatically.", "", "dim", -1, -1, 0)
		}
	}
}
func (m *model) description(row int) {
	m.add(fmt.Sprintf("│    recent window · %d records · N/P page history in transcript", len(m.page.Items)), "dim", row, -1, "")
	recap, assistant := -1, -1
	streams := map[string]bool{}
	for i, e := range m.page.Items {
		streams[fmt.Sprintf("%s/%d", e.Stream, e.Epoch)] = true
		if !e.Current {
			continue
		}
		if recap < 0 && e.Kind == "native_recap" && strings.TrimSpace(e.Text) != "" {
			recap = i
		}
		if assistant < 0 && e.Kind == "message" && e.Role == "assistant" && strings.TrimSpace(e.Text) != "" {
			assistant = i
		}
	}
	if len(streams) > 1 {
		m.add("│    ordering ambiguous across streams · excerpts below follow stream order", "warn", row, -1, "")
	}
	chosen := recap
	label := "native recap"
	if assistant >= 0 && (recap < 0 || assistant < recap) {
		chosen = assistant
		label = "assistant excerpt"
	}
	if chosen < 0 {
		m.add("│    description unavailable in this window · native title shown above", "dim", row, -1, "")
		return
	}
	if len(streams) > 1 {
		label = "stream excerpt (no global latest claim)"
	}
	e := m.page.Items[chosen]
	m.add("│    "+label+" · "+sourceTime(e)+" · coverage "+e.Coverage, "amber", row, -1, "")
	m.paragraph(e.Text, "│    ", "", row, -1, 5)
	if recap >= 0 && recap != chosen && len(streams) == 1 {
		e = m.page.Items[recap]
		m.add("│", "dim", row, -1, "")
		m.add("│    prior native recap · coverage "+e.Coverage, "dim", row, -1, "")
		m.paragraph(e.Text, "│    ", "dim", row, -1, 4)
	}
	if recap > 0 && len(streams) == 1 {
		for _, newer := range m.page.Items[:recap] {
			if newer.Kind == "message" || newer.Kind == "tool_call" || newer.Kind == "tool_result" {
				m.add("│    newer activity follows the recap in this stream window", "warn", row, -1, "")
				break
			}
		}
	}
	if m.page.Next != "" {
		m.add("│    earlier context outside this window · read transcript for older pages", "dim", row, -1, "")
	}
	m.add("│", "dim", row, -1, "")
}
func (m *model) transcriptLines() {
	if m.loading && len(m.page.Items) == 0 {
		m.add("Loading transcript…", "dim", -1, -1, "")
		return
	}
	if len(m.page.Items) == 0 {
		m.add("No records in this view", "dim", -1, -1, "")
		return
	}
	stream := ""
	epoch := int64(-1)
	for i := len(m.page.Items) - 1; i >= 0; i-- {
		e := m.page.Items[i]
		if e.Stream != stream || e.Epoch != epoch {
			if stream != "" {
				m.add("", "", -1, -1, "")
			}
			m.add("── "+position(e)+" · source order; cross-stream order unknown ──", "dim", -1, i, "")
			stream, epoch = e.Stream, e.Epoch
		}
		mark := ""
		color := "dim"
		label := role(e)
		if e.Kind == "message" && e.Text != "" {
			mark = "turn"
			color = "bright"
		}
		if e.Role == "user" {
			color = "amber"
		}
		if e.Kind == "native_recap" {
			mark = "recap"
			color = "amber"
			label = "── native recap · coverage " + e.Coverage
		}
		if e.Kind == "lifecycle_observation" {
			label = "native " + e.State + " observation · historical evidence · session end unknown"
		}
		if e.Kind == "compaction_marker" {
			label = "compaction marker · " + e.Availability.State + " · coverage " + e.Coverage
		}
		if e.Kind == "opaque_record" {
			label = "opaque record · " + e.NativeKind + " · original retained"
		}
		if !e.Current {
			label += " · historical revision"
		}
		m.add(label+"   "+sourceTime(e)+fmt.Sprintf("   #%d", e.Ordinal), color, -1, i, mark)
		if e.Text != "" {
			m.paragraph(e.Text, "  ", "", -1, i, 0)
		}
		for _, p := range e.Parts {
			label := "  ▸ " + single(p.Type)
			if p.Name != "" {
				label += " · " + single(p.Name)
			}
			if !m.tools {
				label += " · space to unfold"
			}
			m.add(label, "dim", -1, i, "")
			if m.tools {
				m.paragraph(p.Text, "    ", "dim", -1, i, 0)
			}
		}
		if e.Truncated {
			m.add("  display clipped · i exact reference · use skald get for the complete record", "warn", -1, i, "")
		}
		m.add("", "", -1, i, "")
	}
	if m.transcriptPage == 0 {
		m.add("── latest archived page · live surface unavailable · session end unknown ──", "dim", -1, -1, "")
	}
}
func (m *model) draw(screen tcell.Screen) {
	oldWidth := m.width
	m.width, m.height = screen.Size()
	anchor, relative := -1, 0
	if m.transcript && (m.reflow || oldWidth != m.width) && m.scroll >= 0 && m.scroll < len(m.lines) {
		anchor = m.lines[m.scroll].entry
		for i := m.scroll - 1; i >= 0 && m.lines[i].entry == anchor; i-- {
			relative++
		}
	}
	m.reflow = false
	w, h := m.width, m.height
	screen.Clear()
	screen.HideCursor()
	if w < 32 || h < 10 {
		put(screen, 0, 0, w, "skald · enlarge terminal to 32×10", m.palette.accent)
		put(screen, 0, 2, w, "q quit", m.palette.dim)
		screen.Show()
		return
	}
	put(screen, 2, 1, w-4, "skald", m.palette.accent.Bold(true))
	state := "daemon offline · cached"
	st := m.palette.bad
	if m.connected {
		state = "daemon connected"
		st = m.palette.green
	}
	put(screen, 10, 1, w-12, state, st)
	if w >= 90 {
		mode := "1 sessions   2 transcript"
		if m.transcript {
			mode = "1 sessions   [2 transcript]"
		} else {
			mode = "[1 sessions]   2 transcript"
		}
		put(screen, w-32, 1, 30, mode, m.palette.dim)
	}
	available, gaps := 0, 0
	for _, s := range m.snapshot.Status.Sources {
		if s.Health == "available" {
			available++
		}
		if s.HasGaps {
			gaps++
		}
	}
	checked := "never checked"
	if !m.checked.IsZero() {
		checked = fmt.Sprintf("checked %ds ago", int(time.Since(m.checked).Seconds()))
	}
	meta := fmt.Sprintf("%d sessions · %d/%d sources available · %d with gaps · %s", m.snapshot.Status.Sessions, available, len(m.snapshot.Status.Sources), gaps, checked)
	if len(m.snapshot.Status.Issues) > 0 {
		meta += fmt.Sprintf(" · %d capture issues (d)", len(m.snapshot.Status.Issues))
	}
	for _, root := range m.snapshot.Status.Discovery {
		if root.State != "available" && root.State != "pending" {
			meta += " · discovery needs attention (d)"
			break
		}
	}
	put(screen, 2, 2, w-4, meta, m.palette.dim)
	title := fmt.Sprintf("sessions / by %s · page %d · filter: %s", groupNames[m.grouping], m.sessionPage+1, m.filter)
	if m.filter == "" {
		title = fmt.Sprintf("sessions / by %s · page %d · %d rows loaded", groupNames[m.grouping], m.sessionPage+1, len(m.rows))
	}
	if m.transcript {
		title = "transcript"
		if s := m.current(); s != nil {
			title += " / " + single(s.Title)
		}
	}
	m.panel(screen, title)
	m.lines = nil
	if m.transcript {
		m.transcriptLines()
		if m.pendingRecap && !m.loading {
			m.scroll = -1
			m.jump("recap", 1)
			m.pendingRecap = false
		}
	} else {
		m.overview()
	}
	if anchor >= 0 {
		for i, l := range m.lines {
			if l.entry == anchor {
				count := 0
				for j := i; j < len(m.lines) && m.lines[j].entry == anchor; j++ {
					count++
				}
				m.scroll = i + min(relative, max(0, count-1))
				break
			}
		}
	}
	bodyHeight := h - 10
	offset := m.scroll
	if !m.transcript {
		start, end := -1, -1
		for i, l := range m.lines {
			if l.row == m.selected {
				if start < 0 {
					start = i
				}
				end = i
			}
		}
		if start >= 0 {
			if start < m.overviewScroll {
				m.overviewScroll = start
			}
			if end >= m.overviewScroll+bodyHeight {
				m.overviewScroll = max(0, min(start, end-bodyHeight+1))
			}
		}
		m.overviewScroll = min(max(0, m.overviewScroll), max(0, len(m.lines)-bodyHeight))
		offset = m.overviewScroll
	} else {
		m.scroll = min(max(0, m.scroll), max(0, len(m.lines)-bodyHeight))
		offset = m.scroll
	}
	for y := 0; y < bodyHeight && offset+y < len(m.lines); y++ {
		l := m.lines[offset+y]
		st := m.palette.tone(l.tone)
		if !m.transcript && l.row == m.selected {
			st = m.palette.selected(st)
			for x := 2; x < w-2; x++ {
				screen.SetContent(x, y+6, ' ', nil, displayStyle(screen, st))
			}
		}
		if m.transcript && m.find != "" && strings.Contains(strings.ToLower(l.text), strings.ToLower(m.find)) {
			st = m.palette.match(st)
		}
		put(screen, 3, y+6, w-6, l.text, st)
		if !m.transcript && l.row == m.selected && m.palette.theme == ThemeDesktop {
			put(screen, 1, y+6, 1, "│", m.palette.accent.Bold(false).Dim(true))
			if strings.HasPrefix(l.text, "▸") {
				put(screen, 3, y+6, 1, "▸", m.palette.accent)
			}
		}
	}
	status := m.notice
	if status == "" {
		if m.transcript {
			kind := "current versions"
			if m.history {
				kind = "all revisions"
			}
			status = fmt.Sprintf("page %d from latest · %s · lines %d–%d / %d", m.transcriptPage+1, kind, min(offset+1, len(m.lines)), min(offset+bodyHeight, len(m.lines)), len(m.lines))
		} else {
			status = "Activity unknown · source health is independent · / filters this page · ? help"
		}
	}
	if m.transcript && m.page.Boundary < m.snapshot.Sessions.Boundary && m.page.Boundary != 0 {
		status = "Archive changed since this page · r refresh · paging restarts if needed"
	}
	if !m.connected && len(m.snapshot.Sessions.Items) > 0 {
		status = "OFFLINE · cached archive view · reconnecting every 5 seconds"
	}
	if m.loading {
		status = "Loading archive page…"
	}
	put(screen, 2, h-3, w-4, status, m.palette.warn)
	keys := "↑↓ select  space expand  Enter read  g group  / filter  N/P pages  ? help  q quit"
	if m.transcript {
		keys = "j/k scroll  [/] turns  n/p recaps  space tools  / find  N/P pages  Esc back  ? help"
	}
	if w < 90 {
		keys = "↑↓ select  Enter read  / filter  ? help  q quit"
		if m.transcript {
			keys = "j/k scroll  space tools  Esc back  ? help  q quit"
		}
	}
	if w < 55 {
		keys = "↑↓ move  Enter read  ? help  q quit"
		if m.transcript {
			keys = "j/k scroll  Esc back  ? help  q quit"
		}
	}
	put(screen, 2, h-2, w-4, keys, m.palette.accent)
	if m.editing {
		label := "filter loaded sessions: "
		if m.transcript {
			label = "find on this transcript page: "
		}
		for x := 2; x < w-2; x++ {
			screen.SetContent(x, h-3, ' ', nil, displayStyle(screen, m.palette.base))
		}
		text := label + single(m.input)
		put(screen, 2, h-3, w-4, text, m.palette.bright)
		screen.ShowCursor(min(w-3, 2+uniseg.StringWidth(text)), h-3)
	}
	if m.help {
		m.drawHelp(screen)
	}
	if m.inspecting {
		m.drawReference(screen)
	}
	if m.diagnostics {
		m.drawDiagnostics(screen)
	}
	screen.Show()
}

// The content panel leaves the connection header and keyboard footer outside
// its outline. Selected rows can tint the left edge without breaking the frame.
func (m *model) panel(screen tcell.Screen, title string) {
	left, right, top, bottom := 1, m.width-2, 4, m.height-4
	style := displayStyle(screen, m.palette.border)
	for x := left + 1; x < right; x++ {
		screen.SetContent(x, top, '─', nil, style)
		screen.SetContent(x, bottom, '─', nil, style)
	}
	for y := top + 1; y < bottom; y++ {
		screen.SetContent(left, y, '│', nil, style)
		screen.SetContent(right, y, '│', nil, style)
	}
	screen.SetContent(left, top, '╭', nil, style)
	screen.SetContent(right, top, '╮', nil, style)
	screen.SetContent(left, bottom, '╰', nil, style)
	screen.SetContent(right, bottom, '╯', nil, style)
	put(screen, 3, top, m.width-6, " "+title+" ", m.palette.accent)
}

func (m *model) overlay(screen tcell.Screen, title string, lines []string) {
	for y := 4; y < m.height-1; y++ {
		for x := 2; x < m.width-2; x++ {
			screen.SetContent(x, y, ' ', nil, displayStyle(screen, m.palette.base))
		}
	}
	m.panel(screen, title)
	var wrapped []string
	for _, text := range lines {
		wrapped = append(wrapped, wrap(text, m.width-8)...)
	}
	height := max(0, m.height-10)
	m.overlayScroll = min(max(0, m.overlayScroll), max(0, len(wrapped)-height))
	for i := 0; i < height && m.overlayScroll+i < len(wrapped); i++ {
		put(screen, 4, 6+i, m.width-8, wrapped[m.overlayScroll+i], m.palette.base)
	}
	put(screen, 3, m.height-2, m.width-6, "↑↓ scroll · Esc close · q quit", m.palette.accent)

}
func (m *model) drawHelp(screen tcell.Screen) {
	m.overlay(screen, "skald / keyboard", []string{
		"Overview", "↑/↓ or j/k  select     space  expand recent context     Enter  transcript", "g  group by project, provider, source or all     /  filter this session page", "h  open recaps     N/P  next/previous session page", "", "Transcript", "j/k  scroll     PgUp/PgDn  page     Home/End or g/G  first/last line", "[/]  previous/next turn     n/p  next/previous native recap on this page", "space  unfold/fold tool details     v  include/exclude historical revisions", "/  find on this page     f/F  next/previous match     N/P  older/newer page", "i  inspect exact record reference     y  copy reference through terminal clipboard", "", "t  cycle heimdall/desktop/amber themes", "r  refresh     d  diagnostics     Esc  back     q or Ctrl-C  quit (daemon keeps running)", "", "Only explicitly configured archives are read. Text and tool payloads are never executed.", "Descriptions use the recent 25-record window. Recap coverage and unknown ordering stay visible.", "Surface activation, Herdr/local groups and ranked search are not available yet.", "", "↑↓ scroll this help; Esc closes it."})
}
func (m *model) drawReference(screen tcell.Screen) {
	e := m.selectedEntry()
	if e == nil {
		m.overlay(screen, "record reference", []string{"No record at this position. Any key returns."})
		return
	}
	m.overlay(screen, "record reference / original retained", []string{"Record: " + e.Key, "Revision: " + e.Revision, "Adapter: " + e.Adapter, "Native kind: " + e.NativeKind, "Position: " + position(*e), "Coverage: " + e.Coverage, "", "Read the complete normalized record using skald get with these exact identifiers.", "Raw bytes additionally require --raw and daemon allow_raw configuration.", "", "y sends this reference to the terminal clipboard; any other key returns."})
}

func (m *model) drawDiagnostics(screen tcell.Screen) {
	s := m.snapshot.Status
	lines := []string{"Theme: " + string(m.palette.theme), fmt.Sprintf("Archive boundary: %d · instance: %s", s.Boundary, s.Instance), "", "Capture health is independent of conversation activity. No verified live surfaces are connected.", ""}
	for _, root := range s.Discovery {
		cutoff := root.ModifiedSince
		if cutoff == "" || strings.HasPrefix(cutoff, "0001-") {
			cutoff = "all files"
		}
		lines = append(lines, "Modified since: "+cutoff)
		lines = append(lines, "Discovery: "+root.Root+" · "+root.State, fmt.Sprintf("%d enrolled / %d limit · %d recent candidates · %d rejected", root.Enrolled, root.MaxSessions, root.Candidates, root.Rejected))
		keys := []string{}
		for path := range root.Issues {
			keys = append(keys, path)
		}
		sort.Strings(keys)
		for _, path := range keys {
			lines = append(lines, path+": "+root.Issues[path])
		}
		lines = append(lines, "")
	}
	for _, source := range s.Sources {
		lines = append(lines, source.Namespace+" / "+source.Provider+" · "+source.Health, "Stream: "+source.Key, fmt.Sprintf("Captured %d bytes · parsed %d bytes · continuity gaps: %t", source.CapturedOffset, source.ParsedOffset, source.HasGaps))
		if source.CheckedAt != nil {
			lines = append(lines, "Source checked: "+*source.CheckedAt)
		}
		if issue := s.Issues[source.Key]; issue != "" {
			lines = append(lines, "Capture issue: "+issue)
		}
		lines = append(lines, "")
	}
	if len(s.Sources) == 0 {
		lines = append(lines, "No source health is currently available.")
	}
	m.overlay(screen, "archive / source diagnostics", lines)
}
