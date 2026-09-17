package tui

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/output"
)

// Attaching a file from the disk: ctrl+o asks for a path on the status line,
// the same idiom as the AI instructions prompt, and ctrl+r takes the last file
// off again. The file is read when it is attached, not when the message is
// sent -- the Files row shows what is going out, size and all, and a file that
// cannot be read says so while there is still a composer to say it in.

// errNothingAttached is ctrl+r with no files on the message.
var errNothingAttached = errors.New("nothing attached to take off")

// startAttach opens the path prompt.
func (c *composeView) startAttach() {
	c.pending, c.err, c.info = pendingNone, nil, ""
	c.attaching, c.path = true, ""
}

// attachKey is a key while the path prompt is open.
func (c *composeView) attachKey(msg tea.KeyPressMsg) {
	c.err, c.info = nil, ""
	switch msg.String() {
	case "enter":
		c.attach(c.path)
		return
	case "esc":
		c.attaching, c.path = false, ""
		return
	case "tab":
		c.completePath()
		return
	case "backspace":
		if r := []rune(c.path); len(r) > 0 {
			c.path = string(r[:len(r)-1])
		}
		return
	case "ctrl+u":
		c.path = ""
		return
	}
	if s := msg.String(); len([]rune(s)) == 1 {
		c.path += s
	} else if msg.Text != "" {
		c.path += msg.Text
	}
}

// attachPaste is a paste while the prompt is open: a path copied from a file
// manager, or a file dropped on the terminal, which arrives the same way --
// quoted, shell-escaped or as a file:// URL depending on who sent it.
func (c *composeView) attachPaste(s string) {
	c.err, c.info = nil, ""
	c.path += cleanPastedPath(s)
}

func cleanPastedPath(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	if rest, ok := strings.CutPrefix(s, "file://"); ok {
		if p, err := url.PathUnescape(rest); err == nil {
			return p
		}
		return rest
	}
	return strings.ReplaceAll(s, `\ `, " ")
}

// attach reads the file and puts it on the message. A path that will not read
// leaves the prompt open with what was typed still in it: the usual failure is
// a typo, and that is fixed by editing, not by starting again.
func (c *composeView) attach(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		c.attaching = false
		return
	}
	a, err := mime.FileAttachment(expandHome(path))
	if err != nil {
		c.err = err
		return
	}
	c.files = append(c.files, a)
	c.filesEdited = true
	c.attaching, c.path = false, ""
	c.info = "attached " + a.Filename + " " + output.HumanSize(int64(len(a.Data))) +
		" · ctrl+r takes it off again"
	c.relayout()
}

// detach is ctrl+r: the last file comes off. Last rather than chosen, because
// the file somebody wants rid of is nearly always the one they just attached
// by mistake; pressing again works back through the row.
func (c *composeView) detach() {
	c.pending, c.err, c.info = pendingNone, nil, ""
	if len(c.files) == 0 {
		c.err = errNothingAttached
		return
	}
	last := c.files[len(c.files)-1]
	c.files = c.files[:len(c.files)-1]
	c.filesEdited = true
	c.info = "took " + last.Filename + " off"
	c.relayout()
}

// relayout has the fields laid out again: the Files row coming or going moves
// the rule, and the body's height with it.
func (c *composeView) relayout() { c.sized = [2]int{} }

// completePath is tab in the prompt: the path grows as far as the disk makes
// it unambiguous, and what it could still become is listed when it is not.
func (c *composeView) completePath() {
	typed := c.path
	matches, _ := filepath.Glob(globEscape(expandHome(typed)) + "*")
	if len(matches) == 0 {
		c.info = "nothing on disk starts with that"
		return
	}
	// Dot files stay out of the way unless the dot was typed, as in a shell.
	if last := typed[strings.LastIndex(typed, "/")+1:]; !strings.HasPrefix(last, ".") {
		shown := matches[:0:0]
		for _, m := range matches {
			if !strings.HasPrefix(filepath.Base(m), ".") {
				shown = append(shown, m)
			}
		}
		if len(shown) > 0 {
			matches = shown
		}
	}
	for i, m := range matches {
		if info, err := os.Stat(m); err == nil && info.IsDir() {
			matches[i] = m + "/"
		}
	}
	common := matches[0]
	for _, m := range matches[1:] {
		common = commonPrefix(common, m)
	}
	// What was typed is kept as typed: "~/Do" completes to "~/Documents/", not
	// to the home directory spelled out.
	if home := homeDir(); home != "" && strings.HasPrefix(typed, "~") {
		if rest, ok := strings.CutPrefix(common, home); ok {
			common = "~" + rest
		}
	}
	if len(common) > len(typed) {
		c.path = common
	}
	if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = filepath.Base(strings.TrimSuffix(m, "/"))
			if strings.HasSuffix(m, "/") {
				names[i] += "/"
			}
		}
		c.info = strings.Join(names, "  ")
	}
}

func commonPrefix(a, b string) string {
	ra, rb := []rune(a), []rune(b)
	n := 0
	for n < len(ra) && n < len(rb) && ra[n] == rb[n] {
		n++
	}
	return string(ra[:n])
}

// globEscape makes a typed path literal to filepath.Glob: a file called
// "notes [final].pdf" is a name, not a character class.
func globEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace(s)
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// expandHome turns a leading ~ into the home directory. Nothing else is
// expanded: the prompt is not a shell.
func expandHome(p string) string {
	home := homeDir()
	if home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(home, rest) + trailingSlash(rest)
	}
	return p
}

// trailingSlash keeps the "/" filepath.Join cleans away, so completing inside
// "~/Documents/" lists the directory rather than its siblings.
func trailingSlash(p string) string {
	if p == "" || strings.HasSuffix(p, "/") {
		return "/"
	}
	return ""
}

// attachFooter is the status line while the prompt is open: the path, then
// whatever tab or a failed read had to say about it. A path longer than the
// line loses its head, not its tail -- the end is where the typing is, and
// where the note that follows it has to stay in sight.
func (c *composeView) attachFooter(w int) string {
	const prefix = "attach · path, tab completes: "
	note := c.info
	if c.err != nil {
		note = c.err.Error()
		// The path is on the line already; what is wanted is what is wrong
		// with it.
		var pe *os.PathError
		if errors.As(c.err, &pe) {
			note = pe.Err.Error()
		}
	}
	if note != "" {
		note = " · " + note
	}
	path := []rune(c.path)
	if room := max(w-len([]rune(prefix))-1-len([]rune(note)), 20); len(path) > room {
		path = append([]rune("…"), path[len(path)-room+1:]...)
	}
	line := padCells(prefix+string(path)+"█"+note, w)
	if c.err != nil {
		return styleErr.Render(line)
	}
	return line
}
