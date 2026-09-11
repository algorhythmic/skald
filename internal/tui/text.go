package tui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"
)

// safeText preserves paragraph breaks and grapheme joiners but makes terminal
// controls and directional overrides visible. Archived bytes are never changed.
func safeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case unicode.IsControl(r) || (unicode.Is(unicode.Cf, r) && r != '\u200d' && r != '\u200c'):
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
func single(s string) string { return strings.ReplaceAll(safeText(s), "\n", " ↵ ") }
func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if uniseg.StringWidth(s) <= w {
		return s
	}
	var b strings.Builder
	used := 0
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		n := g.Width()
		if used+n > w-1 {
			break
		}
		b.WriteString(g.Str())
		used += n
	}
	b.WriteRune('…')
	return b.String()
}
func wrap(s string, w int) []string {
	w = max(1, w)
	var out []string
	for _, paragraph := range strings.Split(safeText(s), "\n") {
		if paragraph == "" {
			out = append(out, "")
			continue
		}
		// Unicode's line break rules keep words together and allow CJK breaks.
		rest := paragraph
		state := -1
		line := ""
		width := 0
		for len(rest) > 0 {
			segment, tail, _, newState := uniseg.FirstLineSegmentInString(rest, state)
			rest, state = tail, newState
			sw := uniseg.StringWidth(segment)
			if width+sw <= w {
				line += segment
				width += sw
				continue
			}
			if width > 0 {
				out = append(out, strings.TrimRight(line, " "))
				line = ""
				width = 0
			}
			if sw <= w {
				line = strings.TrimLeft(segment, " ")
				width = uniseg.StringWidth(line)
				continue
			}
			g := uniseg.NewGraphemes(segment)
			for g.Next() {
				n := g.Width()
				if width+n > w && width > 0 {
					out = append(out, line)
					line = ""
					width = 0
				}
				if n > w {
					line += "�"
					width++
				} else {
					line += g.Str()
					width += n
				}
			}
		}
		if line != "" {
			out = append(out, strings.TrimRight(line, " "))
		}
	}
	return out
}
func put(screen tcell.Screen, x, y, w int, s string, style tcell.Style) {
	style = displayStyle(screen, style)
	s = clip(single(s), w)
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		r := g.Runes()
		n := g.Width()
		if n == 0 {
			continue
		}
		if len(r) > 0 {
			screen.SetContent(x, y, r[0], r[1:], style)
		}
		x += n
	}
}

// span renders a run of text in one style; putSpans lays out a status or key
// line as alternating accents and descriptions like heimdall's footer.
type span struct {
	text  string
	style tcell.Style
}

func putSpans(screen tcell.Screen, x, y, w int, spans []span) {
	cx := x
	for _, sp := range spans {
		rem := x + w - cx
		if rem <= 0 {
			return
		}
		s := clip(single(sp.text), rem)
		put(screen, cx, y, rem, s, sp.style)
		cx += uniseg.StringWidth(s)
	}
}

// Avoid tcell's brightness-based monochrome inversion. NO_COLOR remains
// respected: only selection uses reverse video, and emphasis uses attributes.
func displayStyle(screen tcell.Screen, style tcell.Style) tcell.Style {
	if screen.Colors() != 0 {
		return style
	}
	fg, bg, _ := style.Decompose()
	style = style.Foreground(tcell.ColorDefault).Background(tcell.ColorDefault)
	switch fg {
	case tcell.NewHexColor(0xe3a23a), tcell.NewHexColor(0xecece3),
		tcell.NewHexColor(0xe5a727), tcell.NewHexColor(0xd36b55):
		style = style.Bold(true)
	}
	switch bg {
	case tcell.NewHexColor(0x282820), tcell.NewHexColor(0x403520),
		tcell.NewHexColor(0x1d232a):
		style = style.Reverse(true)
	}
	return style
}
