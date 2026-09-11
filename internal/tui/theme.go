package tui

import (
	"errors"

	"github.com/gdamore/tcell/v2"
)

type Theme string

const (
	ThemeHeimdall Theme = "heimdall"
	ThemeDesktop  Theme = "desktop"
	ThemeAmber    Theme = "amber"
)

func ParseTheme(value string) (Theme, error) {
	switch Theme(value) {
	case ThemeHeimdall, ThemeDesktop, ThemeAmber:
		return Theme(value), nil
	default:
		return "", errors.New("invalid_theme; use_heimdall_desktop_or_amber")
	}
}

type palette struct {
	theme                                               Theme
	base, bright, accent, dim, green, warn, bad, border tcell.Style
}

func paletteFor(theme Theme) palette {
	if theme == ThemeHeimdall {
		// Heimdall's dark palette: cool black background, warm cream text, gold
		// accents, red reserved for failures such as a disconnected daemon.
		base := tcell.StyleDefault.Background(tcell.NewHexColor(0x0e1012)).Foreground(tcell.NewHexColor(0xd4cfc3))
		return palette{theme: theme, base: base, bright: base.Bold(true),
			accent: base.Foreground(tcell.NewHexColor(0xe5a727)), dim: base.Foreground(tcell.NewHexColor(0x78818e)),
			green: base.Foreground(tcell.NewHexColor(0x80ac68)), warn: base.Foreground(tcell.NewHexColor(0xe5a727)),
			bad:    base.Foreground(tcell.NewHexColor(0xd36b55)),
			border: base.Foreground(tcell.NewHexColor(0x394048))}
	}
	if theme == ThemeAmber {
		base := tcell.StyleDefault.Background(tcell.NewHexColor(0x1b1b18)).Foreground(tcell.NewHexColor(0xa7a79a))
		return palette{theme: theme, base: base, bright: base.Foreground(tcell.NewHexColor(0xecece3)),
			accent: base.Foreground(tcell.NewHexColor(0xe3a23a)), dim: base.Foreground(tcell.NewHexColor(0x77776d)),
			green: base.Foreground(tcell.NewHexColor(0x8fc96b)), warn: base.Foreground(tcell.NewHexColor(0xe7c25a)),
			bad:    base.Foreground(tcell.NewHexColor(0xd36b55)),
			border: base.Foreground(tcell.NewHexColor(0x4a4a43))}
	}
	// Leave foreground/background unspecified so both light and dark terminal
	// themes work. ANSI palette references stay references, not captured RGB values,
	// allowing the terminal to apply palette changes to an already-running TUI.
	base := tcell.StyleDefault
	return palette{theme: ThemeDesktop, base: base, bright: base.Bold(true),
		accent: base.Foreground(tcell.PaletteColor(3)).Bold(true), dim: base.Dim(true),
		green: base.Foreground(tcell.PaletteColor(2)), warn: base.Foreground(tcell.PaletteColor(3)),
		bad:    base.Foreground(tcell.PaletteColor(1)),
		border: base.Dim(true)}
}
func (p palette) tone(s string) tcell.Style {
	switch s {
	case "bright":
		return p.bright
	case "amber":
		return p.accent
	case "dim":
		return p.dim
	case "green":
		return p.green
	case "warn":
		return p.warn
	case "bad":
		return p.bad
	}
	return p.base
}

// focusBorder styles the panel edge beside the selected block. Heimdall marks
// the focused section with a full gold border; desktop keeps a subdued line.
func (p palette) focusBorder() tcell.Style {
	if p.theme == ThemeHeimdall {
		return p.accent
	}
	return p.accent.Bold(false).Dim(true)
}
func (p palette) selected(style tcell.Style) tcell.Style {
	if p.theme == ThemeHeimdall {
		return style.Background(tcell.NewHexColor(0x1d232a))
	}
	if p.theme == ThemeAmber {
		return style.Background(tcell.NewHexColor(0x282820))
	}
	// Keep the terminal background and text hierarchy. The selection marker and
	// slim gutter in the renderer identify focus without a bright inverted block.
	return style.Background(tcell.ColorDefault).Reverse(false)
}
func (p palette) match(style tcell.Style) tcell.Style {
	if p.theme == ThemeHeimdall {
		return style.Foreground(tcell.NewHexColor(0xe5a727)).Background(tcell.NewHexColor(0x1d232a))
	}
	if p.theme == ThemeAmber {
		return style.Background(tcell.NewHexColor(0x403520))
	}
	return style.Underline(true).Bold(true)
}
