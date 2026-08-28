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

// wrapCells breaks s into lines of at most n terminal cells, at spaces where
// it can and mid-word when a single word is wider than the line. Blank lines
// survive: paragraph breaks are most of what makes a mail body readable, and
// the expanded thread view stacks bodies with nothing but those breaks between
// their parts.
func wrapCells(s string, n int) []string {
	if n <= 0 {
		return nil
	}
	var out []string
	for _, para := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		para = strings.TrimRight(strings.ReplaceAll(para, "\t", "    "), " ")
		if para == "" {
			out = append(out, "")
			continue
		}
		// Leading whitespace is kept — it is what tells a list apart from a
		// paragraph — but never enough of it to leave no room for text.
		indent := para[:len(para)-len(strings.TrimLeft(para, " "))]
		if runewidth.StringWidth(indent) >= n {
			indent = ""
		}
		room := n - runewidth.StringWidth(indent)
		line := indent
		for _, word := range strings.Fields(para) {
			for runewidth.StringWidth(word) > room {
				if line != indent {
					out = append(out, line)
					line = indent
				}
				var head string
				head, word = cutCells(word, room)
				out = append(out, indent+head)
			}
			switch {
			case line == indent:
				line += word
			case runewidth.StringWidth(line)+1+runewidth.StringWidth(word) <= n:
				line += " " + word
			default:
				out = append(out, line)
				line = indent + word
			}
		}
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// cutCells splits s after at most n terminal cells. The head is never empty:
// a rune wider than the whole column has to overflow it, because returning
// nothing would leave the caller cutting the same word forever.
func cutCells(s string, n int) (head, rest string) {
	w := 0
	for i, r := range s {
		rw := runewidth.RuneWidth(r)
		if w+rw > n && i > 0 {
			return s[:i], s[i:]
		}
		w += rw
		if w >= n {
			return s[:i+len(string(r))], s[i+len(string(r)):]
		}
	}
	return s, ""
}

// tabStrip is the header's leading run of tabs. It exists because the two
// stacks are otherwise undiscoverable: the header used to show only the
// current screen's title, so someone in the mail list had no way of knowing a
// calendar was one key away. It returns the styled strip and its cell width,
// which the caller needs to pad the rest of the line.
func tabStrip(onCal bool) (string, int) {
	const mail, cal = " 1 mail ", " 2 calendar "
	w := runewidth.StringWidth(mail) + runewidth.StringWidth(cal)
	if onCal {
		return styleFaint.Render(mail) + styleSelected.Render(cal), w
	}
	return styleSelected.Render(mail) + styleFaint.Render(cal), w
}
