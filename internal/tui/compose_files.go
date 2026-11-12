package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-runewidth"

	"github.com/teulaert/emlcalsync/internal/output"
)

// The Files row is a stop on the tab ring once there is a file on the
// message, the way To and Cc are. On it the arrows walk the files, enter
// opens the one under the cursor with whatever the desktop opens that kind of
// file with, w saves it to the downloads folder, and delete takes it off.
//
// The row exists to be checked before ctrl+d -- is that the right photo, the
// signed version of the contract -- and a name and a size do not answer that.
// The bytes are in the composer already, so opening one is writing it out,
// not fetching it: a file put on with ctrl+o a moment ago opens the same as
// one a stored draft came back with.

// filesKey is a key while the Files row has focus.
func (c *composeView) filesKey(press tea.KeyPressMsg) tea.Cmd {
	c.pending, c.err, c.info = pendingNone, nil, ""
	c.fileAt = min(max(c.fileAt, 0), max(len(c.files)-1, 0))
	if len(c.files) == 0 {
		return nil
	}
	switch press.String() {
	case "left", "up", "h", "k":
		c.fileAt = max(c.fileAt-1, 0)
	case "right", "down", "l", "j", "space":
		c.fileAt = min(c.fileAt+1, len(c.files)-1)
	case "home":
		c.fileAt = 0
	case "end":
		c.fileAt = len(c.files) - 1
	case "enter", "o":
		c.info = "opening " + c.files[c.fileAt].Filename + "…"
		return c.d.openDraftFile(c.files[c.fileAt], false)
	case "w":
		c.info = "saving " + c.files[c.fileAt].Filename + "…"
		return c.d.openDraftFile(c.files[c.fileAt], true)
	case "delete", "backspace", "x":
		c.takeOff(c.fileAt)
	}
	return nil
}

// takeOff removes the i'th file from the message. When that was the last one
// the row goes, and the cursor with it, into the body: a focus left on a row
// that is no longer drawn is a cursor nowhere.
func (c *composeView) takeOff(i int) {
	if i < 0 || i >= len(c.files) {
		return
	}
	gone := c.files[i]
	c.files = append(c.files[:i:i], c.files[i+1:]...)
	c.filesEdited = true
	c.info = "took " + gone.Filename + " off"
	if c.fileAt >= len(c.files) {
		c.fileAt = max(len(c.files)-1, 0)
	}
	if len(c.files) == 0 && c.focus == filesFocus {
		c.focusField(bodyFocus)
	}
	c.relayout()
}

// filesFooter is the status line while the Files row has focus.
func (c *composeView) filesFooter() string {
	return "← / → file · enter open · w save to " + shortHome(c.d.downloadDir()) +
		" · del take off · tab: on to the body"
}

// filesView is the attachments row: what is going out with the message, and
// what could not be got. With the focus on it, the file under the cursor is
// marked, and the row slides along so that one stays in sight.
func (c *composeView) filesView(w int) string {
	room := max(w-labelW, 0)
	names := make([]string, 0, len(c.files))
	for _, f := range c.files {
		names = append(names, f.Filename+" "+output.HumanSize(int64(len(f.Data))))
	}
	note := c.filesNote
	if note != "" && len(names) > 0 {
		note = " · " + note
	}
	// The whole row is marked once something is missing from it: the point is
	// that the eye stops here before ctrl+d, not that each name is graded.
	paint := func(s string) string {
		if c.filesNote != "" {
			return styleErr.Render(s)
		}
		return s
	}
	if c.focus != filesFocus || len(names) == 0 {
		return paint(padCells(strings.Join(names, ", ")+note, room))
	}

	at := min(max(c.fileAt, 0), len(names)-1)
	start := 0
	for start < at && runewidth.StringWidth("… "+strings.Join(names[start:at+1], ", ")) > room {
		start++
	}
	before := strings.Join(names[start:at], ", ")
	if before != "" {
		before += ", "
	}
	if start > 0 {
		before = "… " + before
	}
	after := ""
	if at+1 < len(names) {
		after = ", " + strings.Join(names[at+1:], ", ")
	}
	after += note

	before = truncCells(before, room)
	left := room - runewidth.StringWidth(before)
	sel := truncCells(names[at], left)
	left -= runewidth.StringWidth(sel)
	return paint(before) + styleSelected.Render(sel) + paint(padCells(after, left))
}
