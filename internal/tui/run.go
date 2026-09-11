package tui

import (
	"context"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/gdamore/tcell/v2"
)

type result struct {
	index    int
	id       int
	kind     string
	snapshot Snapshot
	page     archive.Page[archive.TranscriptEntry]
	key      string
	err      error
}

func Run(ctx context.Context, backend Backend, theme Theme) error {
	screen, err := tcell.NewScreen()
	if err != nil {
		return err
	}
	if err = screen.Init(); err != nil {
		return err
	}
	defer screen.Fini()
	return runScreen(ctx, screen, backend, theme)
}
func runScreen(ctx context.Context, screen tcell.Screen, backend Backend, theme Theme) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan tcell.Event, 32)
	quitEvents := make(chan struct{})
	defer close(quitEvents)
	go screen.ChannelEvents(events, quitEvents)
	results := make(chan result, 4)
	m := newModel()
	m.palette = paletteFor(theme)
	screen.SetStyle(displayStyle(screen, m.palette.base))
	screen.HideCursor()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	snapshotID, detailID := 0, 0
	var snapshotCancel, detailCancel context.CancelFunc
	snapshotBusy := false
	fetchSnapshot := func(force bool) {
		if snapshotBusy && !force {
			return
		}
		if snapshotCancel != nil {
			snapshotCancel()
		}
		snapshotID++
		id := snapshotID
		snapshotBusy = true
		index := m.sessionTarget
		cursor := m.sessionCursors[index]
		task, stop := context.WithTimeout(ctx, 12*time.Second)
		snapshotCancel = stop
		go func() {
			defer stop()
			value, err := backend.Snapshot(task, cursor)
			select {
			case results <- result{id: id, index: index, kind: "snapshot", snapshot: value, err: err}:
			case <-ctx.Done():
			}
		}()
	}
	fetchDetail := func() {
		if detailCancel != nil {
			detailCancel()
		}
		detailID++
		s := m.current()
		if s == nil {
			return
		}
		index := m.transcriptTarget
		id, key, cursor, history := detailID, s.Key, m.transcriptCursors[index], m.history
		m.loading = true
		m.detailKey = key
		task, stop := context.WithTimeout(ctx, 12*time.Second)
		detailCancel = stop
		go func() {
			defer stop()
			page, err := backend.Transcript(task, key, cursor, history)
			select {
			case results <- result{id: id, index: index, kind: "detail", key: key, page: page, err: err}:
			case <-ctx.Done():
			}
		}()
	}
	defer func() {
		if snapshotCancel != nil {
			snapshotCancel()
		}
		if detailCancel != nil {
			detailCancel()
		}
	}()
	fetchSnapshot(false)
	for {
		m.draw(screen)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			fetchSnapshot(false)
		case r := <-results:
			if (r.kind == "snapshot" && r.id != snapshotID) || (r.kind == "detail" && r.id != detailID) {
				continue
			}
			if r.kind == "snapshot" {
				snapshotBusy = false
			} else {
				m.loading = false
			}
			if r.err != nil {
				code := r.err.Error()
				if code == "scope_denied" || (r.kind == "detail" && code == "not_found") {
					snapshotID++
					snapshotBusy = false
					if snapshotCancel != nil {
						snapshotCancel()
					}
					m.clearPrivate()
					detailID++
					if detailCancel != nil {
						detailCancel()
					}
					m.connected = false
					m.notice = "Access denied · cached conversation content cleared"
					continue
				}
				if code == "cursor_expired" {
					m.notice = "Archive changed · page restarted to keep a consistent view"
					if r.kind == "snapshot" {
						m.sessionCursors = []string{""}
						m.sessionPage = 0
						m.sessionTarget = 0
						fetchSnapshot(true)
					} else {
						m.resetDetail()
						fetchDetail()
					}
					continue
				}
				if code == "daemon_unavailable" || code == "context deadline exceeded" {
					m.connected = false
					m.notice = "Daemon unavailable · cached view · retrying every 5s"
				} else {
					m.notice = "Archive read failed: " + single(code)
				}
				continue
			}
			if r.kind == "snapshot" {
				reconnect := !m.connected
				instanceChanged := m.snapshot.Status.Instance != "" && m.snapshot.Status.Instance != r.snapshot.Status.Instance
				oldKey := ""
				if s := m.current(); s != nil {
					oldKey = s.Key
				}
				m.connected = true
				m.checked = time.Now()
				m.sessionPage = r.index
				m.snapshot = r.snapshot
				m.rebuildRows()
				s := m.current()
				if s == nil || s.Key != oldKey || instanceChanged {
					m.resetDetail()
					detailID++
					if detailCancel != nil {
						detailCancel()
					}
					m.loading = false
				}
				if reconnect {
					m.notice = "Connected · surface activation and ranked search unavailable"
				}
				if (m.expanded || m.transcript) && s != nil && (reconnect || instanceChanged || m.detailKey == "" || (m.expanded && !m.transcript && !m.loading && m.page.Boundary != m.snapshot.Sessions.Boundary)) {
					m.resetDetail()
					fetchDetail()
				}
			} else {
				s := m.current()
				if s == nil || s.Key != r.key {
					continue
				}
				m.transcriptPage = r.index
				m.page = r.page
				m.detailKey = r.key
				m.pageKey = r.key
				m.notice = ""
			}
		case event := <-events:
			switch ev := event.(type) {
			case *tcell.EventResize:
				screen.Sync()
			case *tcell.EventKey:
				if ev.Key() == tcell.KeyCtrlC {
					return nil
				}
				if m.editing {
					switch ev.Key() {
					case tcell.KeyEscape:
						m.input = m.previous
						m.editing = false
					case tcell.KeyEnter:
						m.editing = false
					case tcell.KeyBackspace, tcell.KeyBackspace2:
						r := []rune(m.input)
						if len(r) > 0 {
							m.input = string(r[:len(r)-1])
						}
					case tcell.KeyRune:
						if len(m.input) < 256 {
							m.input += string(ev.Rune())
						}
					}
					if m.transcript {
						m.find = m.input
						if !m.editing {
							m.findNext(1)
						}
					} else {
						m.filter = m.input
						m.rebuildRows()
						m.overviewScroll = 0
						m.resetDetail()
						detailID++
						if detailCancel != nil {
							detailCancel()
						}
						m.loading = false
						if !m.editing && m.expanded {
							fetchDetail()
						}
					}
					continue
				}
				if m.help || m.inspecting || m.diagnostics {
					switch {
					case ev.Rune() == 'q':
						return nil
					case ev.Key() == tcell.KeyDown || ev.Rune() == 'j':
						m.overlayScroll++
					case ev.Key() == tcell.KeyUp || ev.Rune() == 'k':
						m.overlayScroll--
					case ev.Key() == tcell.KeyPgDn:
						m.overlayScroll += max(1, m.height-10)
					case ev.Key() == tcell.KeyPgUp:
						m.overlayScroll -= max(1, m.height-10)
					case m.inspecting && ev.Rune() == 'y':
						m.copyReference(screen)
					default:
						m.help = false
						m.inspecting = false
						m.diagnostics = false
						m.overlayScroll = 0
					}
					continue
				}

				key := ev.Rune()
				if key == 'q' {
					return nil
				}
				if key == 't' {
					next := ThemeDesktop
					switch m.palette.theme {
					case ThemeDesktop:
						next = ThemeAmber
					case ThemeAmber:
						next = ThemeHeimdall
					}
					m.palette = paletteFor(next)
					screen.SetStyle(displayStyle(screen, m.palette.base))
					m.notice = "Theme: " + string(next)
					screen.Sync()
					continue
				}
				if key == 'd' {
					m.diagnostics = true
					m.overlayScroll = 0
					continue
				}
				if key == '?' {
					m.overlayScroll = 0
					m.help = true
					continue
				}
				if key == '/' {
					m.editing = true
					if m.transcript {
						m.input = m.find
					} else {
						m.input = m.filter
					}
					m.previous = m.input
					continue
				}
				if key == 'o' {
					m.notice = "Surface activation unavailable · no verified live target"
					continue
				}
				if key == 's' {
					m.notice = "Ranked search unavailable · / filters the loaded session page or finds transcript text"
					continue
				}
				if key == 'r' {
					fetchSnapshot(true)
					if m.expanded || m.transcript {
						m.resetDetail()
						fetchDetail()
					}
					continue
				}
				if ev.Key() == tcell.KeyEscape || key == '1' {
					if m.transcript && (m.transcriptPage > 0 || m.history || m.page.Boundary != m.snapshot.Sessions.Boundary) {
						m.history = false
						m.resetDetail()
						if m.expanded {
							fetchDetail()
						}
					}
					m.transcript = false
					m.inspecting = false
					m.scroll = 0
					continue
				}
				if m.transcript {
					switch {
					case key == 'j' || ev.Key() == tcell.KeyDown:
						m.scroll++
					case key == 'k' || ev.Key() == tcell.KeyUp:
						m.scroll--
					case ev.Key() == tcell.KeyPgDn || ev.Key() == tcell.KeyCtrlD:
						m.scroll += max(1, m.height-9)
					case ev.Key() == tcell.KeyPgUp || ev.Key() == tcell.KeyCtrlU:
						m.scroll -= max(1, m.height-9)
					case ev.Key() == tcell.KeyHome || key == 'g':
						m.scroll = 0
					case ev.Key() == tcell.KeyEnd || key == 'G':
						m.scroll = max(0, len(m.lines)-1)
					case key == 'n':
						m.jump("recap", 1)
					case key == 'p':
						m.jump("recap", -1)
					case key == ']':
						m.jump("turn", 1)
					case key == '[':
						m.jump("turn", -1)
					case key == 'f':
						m.findNext(1)
					case key == 'F':
						m.findNext(-1)
					case key == ' ':
						m.tools = !m.tools
						m.reflow = true
					case key == 'i':
						m.inspecting = true
						m.overlayScroll = 0
					case key == 'y':
						m.copyReference(screen)
					case key == 'v':
						m.history = !m.history
						m.resetDetail()
						fetchDetail()
					case key == 'N' && !m.loading:
						if m.page.Next != "" {
							m.transcriptCursors = append(m.transcriptCursors[:m.transcriptPage+1], m.page.Next)
							m.transcriptTarget = m.transcriptPage + 1
							m.scroll = 0
							fetchDetail()
						} else {
							m.notice = "Beginning of archived history"
						}
					case key == 'P' && !m.loading:
						if m.transcriptPage > 0 {
							m.transcriptTarget = m.transcriptPage - 1
							m.scroll = 0
							fetchDetail()
						} else {
							m.notice = "Latest archive page"
						}
					}
				} else {
					moved := false
					switch {
					case key == 'j' || ev.Key() == tcell.KeyDown:
						m.move(1)
						moved = true
					case key == 'k' || ev.Key() == tcell.KeyUp:
						m.move(-1)
						moved = true
					case ev.Key() == tcell.KeyPgDn:
						m.move(max(1, (m.height-10)/3))
						moved = true
					case ev.Key() == tcell.KeyPgUp:
						m.move(-max(1, (m.height-10)/3))
						moved = true
					case ev.Key() == tcell.KeyHome:
						m.move(-len(m.rows))
						moved = true
					case ev.Key() == tcell.KeyEnd:
						m.move(len(m.rows))
						moved = true
					case key == 'g':
						oldKey := ""
						if s := m.current(); s != nil {
							oldKey = s.Key
						}
						m.grouping = (m.grouping + 1) % len(groupNames)
						m.rebuildRows()
						m.overviewScroll = 0
						if s := m.current(); s == nil || s.Key != oldKey {
							m.resetDetail()
							moved = true
						}
					case key == 'O':
						oldKey := ""
						if s := m.current(); s != nil {
							oldKey = s.Key
						}
						m.ordering = (m.ordering + 1) % len(orderNames)
						m.rebuildRows()
						m.overviewScroll = 0
						if s := m.current(); s == nil || s.Key != oldKey {
							m.resetDetail()
							moved = true
						}
					case key == 'x':
						oldKey := ""
						if s := m.current(); s != nil {
							oldKey = s.Key
						}
						m.hideIdle = !m.hideIdle
						m.rebuildRows()
						if s := m.current(); s == nil || s.Key != oldKey {
							m.resetDetail()
							moved = true
						}
					case key == ' ':
						if m.selected >= 0 && m.selected < len(m.rows) && m.rows[m.selected].cluster != nil {
							k := clusterKey(m.rows[m.selected].group, m.rows[m.selected].cluster)
							if m.expandedClusters == nil {
								m.expandedClusters = map[string]bool{}
							}
							m.expandedClusters[k] = !m.expandedClusters[k]
							m.rebuildRows()
						} else {
							m.expanded = !m.expanded
							if m.expanded {
								fetchDetail()
							}
						}
					case ev.Key() == tcell.KeyEnter || key == 'h' || key == '2':
						if m.selected >= 0 && m.selected < len(m.rows) && m.rows[m.selected].cluster != nil && key != 'h' {
							k := clusterKey(m.rows[m.selected].group, m.rows[m.selected].cluster)
							if m.expandedClusters == nil {
								m.expandedClusters = map[string]bool{}
							}
							m.expandedClusters[k] = !m.expandedClusters[k]
							m.rebuildRows()
						} else if m.current() != nil {
							m.transcript = true
							m.pendingRecap = key == 'h'
							m.scroll = 0
							if m.detailKey == "" {
								fetchDetail()
							}
						}
					case key == 'N':
						if m.snapshot.Sessions.Next != "" {
							m.sessionCursors = append(m.sessionCursors[:m.sessionPage+1], m.snapshot.Sessions.Next)
							m.sessionTarget = m.sessionPage + 1
							fetchSnapshot(true)
						} else {
							m.notice = "No more sessions"
						}
					case key == 'P':
						if m.sessionPage > 0 {
							m.sessionTarget = m.sessionPage - 1
							fetchSnapshot(true)
						}
					}
					if moved {
						detailID++
						if detailCancel != nil {
							detailCancel()
						}
						m.loading = false
						if m.expanded {
							fetchDetail()
						}
					}
				}
			}
		}
	}
}
