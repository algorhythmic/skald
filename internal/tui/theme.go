package tui

import (
	"errors"

	"github.com/gdamore/tcell/v2"
)

type Theme string

const (
	ThemeDesktop Theme = "desktop"
	ThemeAmber   Theme = "amber"
)

func ParseTheme(value string) (Theme, error) {
	switch Theme(value) {
	case ThemeDesktop, ThemeAmber:
		return Theme(value), nil
	default:
		return "", errors.New("invalid_theme; use_desktop_or_amber")
	}
}

type palette struct {
	theme                                          Theme
	base, bright, accent, dim, green, warn, border tcell.Style
}

func paletteFor(theme Theme) palette {
	if theme == ThemeAmber {
		base := tcell.StyleDefault.Background(tcell.NewHexColor(0x1b1b18)).Foreground(tcell.NewHexColor(0xa7a79a))
		return palette{theme: theme, base: base, bright: base.Foreground(tcell.NewHexColor(0xecece3)),
			accent: base.Foreground(tcell.NewHexColor(0xe3a23a)), dim: base.Foreground(tcell.NewHexColor(0x77776d)),
			green: base.Foreground(tcell.NewHexColor(0x8fc96b)), warn: base.Foreground(tcell.NewHexColor(0xe7c25a)),
			border: base.Foreground(tcell.NewHexColor(0x4a4a43))}
	}
	// Leave foreground/background unspecified so both light and dark terminal
	// themes work. ANSI palette references stay references, not captured RGB values,
	// allowing the terminal to apply palette changes to an already-running TUI.
	base := tcell.StyleDefault
	return palette{theme: ThemeDesktop, base: base, bright: base.Bold(true),
		accent: base.Foreground(tcell.PaletteColor(3)).Bold(true), dim: base.Dim(true),
		green: base.Foreground(tcell.PaletteColor(2)), warn: base.Foreground(tcell.PaletteColor(3)),
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
	}
	return p.base
}
func (p palette) selected(style tcell.Style) tcell.Style {
	if p.theme == ThemeAmber {
		return style.Background(tcell.NewHexColor(0x282820))
	}
	// Keep the terminal background and text hierarchy. The selection marker and
	// slim gutter in the renderer identify focus without a bright inverted block.
	return style.Background(tcell.ColorDefault).Reverse(false)
}
func (p palette) match(style tcell.Style) tcell.Style {
	if p.theme == ThemeAmber {
		return style.Background(tcell.NewHexColor(0x403520))
	}
	return style.Underline(true).Bold(true)
}
