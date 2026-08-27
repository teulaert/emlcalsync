package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
)

// Styles lean on the terminal's own attributes rather than a fixed palette:
// reverse video for the cursor, bold for unread, faint for chrome. That reads
// correctly on a light and a dark terminal without asking which one it is,
// which matters because the rest of emlcal emits no colour at all today.
var (
	styleSelected = lipgloss.NewStyle().Reverse(true)
	styleUnread   = lipgloss.NewStyle().Bold(true)
	styleFaint    = lipgloss.NewStyle().Faint(true)
	styleHeader   = lipgloss.NewStyle().Bold(true)
	styleErr      = lipgloss.NewStyle().Bold(true)
)

// truncCells shortens s to at most n terminal cells, ending with "…" when it
// had to cut.
//
// output.Truncate counts runes, which is right for a byte-safe cut but wrong
// for alignment: a CJK ideograph or an emoji occupies two cells, so a column
// budgeted in runes overflows and every column to its right shifts. The CLI
// tables have always had this bug; here the columns are drawn every frame
// against a real terminal width, so it is visible immediately.
func truncCells(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	if runewidth.StringWidth(s) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if w+rw > n-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return strings.TrimRight(b.String(), " ") + "…"
}

// runeCells is the terminal width of one rune.
func runeCells(r rune) int { return runewidth.RuneWidth(r) }

// padCells right-pads s to exactly n terminal cells (truncating if needed), so
// a row of them lines up whatever alphabet the mail is written in.
func padCells(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = truncCells(s, n)
	if d := n - runewidth.StringWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// frame stacks a header, a body of at most n lines and a footer into exactly
// h lines, so a screen never scrolls the terminal by overshooting.
func frame(header string, body []string, footer string, w, h int) string {
	var out []string
	if header != "" {
		out = append(out, padCells(header, w))
	}
	room := h - len(out)
	if footer != "" {
		room--
	}
	if room < 0 {
		room = 0
	}
	for i := 0; i < room; i++ {
		if i < len(body) {
			out = append(out, body[i])
		} else {
			out = append(out, "")
		}
	}
	if footer != "" {
		out = append(out, padCells(footer, w))
	}
	return strings.Join(out, "\n")
}
