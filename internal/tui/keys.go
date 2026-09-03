package tui

import "charm.land/bubbles/v2/key"

// keymap is the whole binding surface, the composer's keys included. The help
// overlay below is written by hand against it -- it carries prose no
// key.WithHelp string would hold -- and a test walks every binding here
// looking for its keys in that text, so a key that works and a key that is
// documented cannot drift apart.
type keymap struct {
	Up, Down    key.Binding
	Top, Bottom key.Binding
	PageUp      key.Binding
	PageDown    key.Binding
	Open        key.Binding
	Back        key.Binding
	LineDown    key.Binding
	LineUp      key.Binding
	Expand      key.Binding
	Quit        key.Binding
	Mail        key.Binding
	Calendar    key.Binding
	Switch      key.Binding
	Search      key.Binding
	Archive     key.Binding
	Trash       key.Binding
	Restore     key.Binding
	ToggleRead  key.Binding
	Star        key.Binding
	Reply       key.Binding
	ReplyAll    key.Binding
	Forward     key.Binding
	New         key.Binding
	Account     key.Binding
	Mailbox     key.Binding
	Refresh     key.Binding
	Undo        key.Binding
	AI          key.Binding
	Copy        key.Binding
	Browser     key.Binding
	BrowserFlip key.Binding
	Help        key.Binding

	// The composer's own keys. They live here with the rest rather than as
	// string cases inside compose.go, because a binding that is not in this
	// struct is one the help overlay cannot be checked against -- which is
	// how shift+tab came to work for a release without being written down
	// anywhere.
	Send        key.Binding
	SaveDraft   key.Binding
	DeleteDraft key.Binding
	NextField   key.Binding
	PrevField   key.Binding
	// The address book under To, Cc and Bcc. They are only looked at while
	// something is being offered; otherwise enter and the arrows are what
	// they were.
	NextHint key.Binding
	PrevHint key.Binding
	TakeHint key.Binding
}

func defaultKeys() keymap {
	return keymap{
		Up:       key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("j/k", "move")),
		Down:     key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/k", "move")),
		Top:      key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g/G", "top / bottom")),
		Bottom:   key.NewBinding(key.WithKeys("G", "end"), key.WithHelp("g/G", "top / bottom")),
		PageUp:   key.NewBinding(key.WithKeys("ctrl+b", "pgup"), key.WithHelp("ctrl+b", "page up")),
		PageDown: key.NewBinding(key.WithKeys("ctrl+f", "pgdown"), key.WithHelp("ctrl+f", "page down")),
		Open:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
		Back:     key.NewBinding(key.WithKeys("esc", "u"), key.WithHelp("esc", "back")),
		LineDown: key.NewBinding(key.WithKeys("J"), key.WithHelp("J/K", "scroll one line")),
		LineUp:   key.NewBinding(key.WithKeys("K"), key.WithHelp("J/K", "scroll one line")),
		Expand:   key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "expand / collapse a thread")),
		Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "back / quit")),
		Mail:     key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "mail")),
		Calendar: key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "calendar")),
		Switch:   key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch screen")),
		Search:   key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Archive:  key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "archive")),
		// delete and backspace are the same key on the two keyboards a
		// terminal is read on, and on both of them it is what a mail client
		// deletes with. d is the one that is typed; these are the one that is
		// reached for.
		Trash:      key.NewBinding(key.WithKeys("d", "delete", "backspace"), key.WithHelp("d", "trash")),
		Restore:    key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "back to the inbox")),
		ToggleRead: key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "read / unread")),
		Star:       key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "star")),
		Reply:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "reply")),
		ReplyAll:   key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "reply to all")),
		Forward:    key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "forward")),
		New:        key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "new message")),
		Account:    key.NewBinding(key.WithKeys("A"), key.WithHelp("A", "account filter")),
		Mailbox:    key.NewBinding(key.WithKeys("M"), key.WithHelp("M", "mailbox")),
		Refresh:    key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh")),
		Undo:       key.NewBinding(key.WithKeys("z"), key.WithHelp("z", "undo")),
		AI:         key.NewBinding(key.WithKeys("ctrl+g"), key.WithHelp("ctrl+g", "ask the AI")),
		Copy:       key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "copy the id")),
		Browser:    key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open in the browser")),
		BrowserFlip: key.NewBinding(key.WithKeys("O"),
			key.WithHelp("O", "open in the browser, the other way on pictures")),
		Help: key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),

		Send:        key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("ctrl+d", "send")),
		SaveDraft:   key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "save as a draft")),
		DeleteDraft: key.NewBinding(key.WithKeys("ctrl+x"), key.WithHelp("ctrl+x", "delete the draft")),
		NextField:   key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next field")),
		PrevField:   key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "previous field")),
		NextHint:    key.NewBinding(key.WithKeys("ctrl+n", "down"), key.WithHelp("ctrl+n", "next address offered")),
		PrevHint:    key.NewBinding(key.WithKeys("ctrl+p", "up"), key.WithHelp("ctrl+p", "previous address offered")),
		TakeHint:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "take the address offered")),
	}
}

// helpLines is the ? overlay. It is prose rather than a rendering of the
// bindings -- what a key is for is not what its name is -- but every key in
// the struct above has to turn up in it somewhere, which TestHelpCoversEveryKey
// is what enforces.
func (k keymap) helpLines() [][2]string {
	return [][2]string{
		{"j / k  ↓ ↑", "move"},
		{"g / G", "top / bottom"},
		{"ctrl+f / ctrl+b", "page down / up"},
		{"enter", "open (a draft: reopen it in the composer)"},
		{"J / K", "scroll one line (an expanded thread)"},
		{"t", "thread: expanded text / one row per message"},
		{"esc / q / u", "back (quit at the top)"},
		{"tab, 1, 2", "switch mail / calendar"},
		{"/", "search (FTS5 syntax)"},
		{"", ""},
		{"e", "archive"},
		{"d  ⌫ / delete", "trash"},
		{"i", "back to the inbox (out of the archive or trash)"},
		{"m", "toggle read / unread"},
		{"s", "toggle star"},
		{"z", "undo the last action"},
		{"ctrl+g", "ask the AI about the conversation: a summary, or a question typed at the prompt"},
		{"y", "copy the id to the clipboard: the thread's on a row, the message's in a thread or the reader"},
		{"o", "open the message in the browser, as the sender wrote it"},
		{"O", "the same, reversing whether the pictures the sender hosts elsewhere are fetched"},
		{"y / n / t", "on an event, or the mail inviting to one: accept / decline / tentative"},
		{"enter", "on an invitation: open the event on the calendar"},
		{"  r", "on the summary: reply to the conversation"},
		{"", ""},
		{"r", "reply (carries on the thread's draft, if there is one)"},
		{"a", "reply to all"},
		{"f", "forward it on, attachments and all"},
		{"c", "a new message, from the account the list is filtered to"},
		{"  ctrl+d", "send it"},
		{"  ctrl+s", "save it as a draft on the server"},
		{"  ctrl+x", "delete the draft being edited (twice)"},
		{"  ctrl+g", "draft it with the AI model: asks for instructions, enter alone just answers"},
		{"  tab / shift+tab", "next / previous field"},
		{"  ctrl+n / ctrl+p, enter", "in To, Cc, Bcc: pick / take an address the archive knows, as it is typed"},
		{"  esc", "cancel (twice, once something is written)"},
		{"", ""},
		{"A", "cycle the account filter"},
		{"M", "cycle the mailbox (inbox, all, flagged, drafts, sent, archive, trash, spam)"},
		{"R", "refresh now (and nudge the sync daemon)"},
		{"?", "this help"},
		{"", ""},
		// The marks, not keys -- and A is both, which is exactly why it is
		// worth spelling out here.
		{"● and A", "row marks: unread · carries an attachment"},
	}
}
