package tui

import "charm.land/bubbles/v2/key"

// keymap is the whole binding surface. The help overlay is generated from it,
// so a key that works and a key that is documented cannot drift apart.
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
	ToggleRead  key.Binding
	Star        key.Binding
	Account     key.Binding
	Mailbox     key.Binding
	Refresh     key.Binding
	Undo        key.Binding
	Help        key.Binding
}

func defaultKeys() keymap {
	return keymap{
		Up:         key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("j/k", "move")),
		Down:       key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/k", "move")),
		Top:        key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g/G", "top / bottom")),
		Bottom:     key.NewBinding(key.WithKeys("G", "end"), key.WithHelp("g/G", "top / bottom")),
		PageUp:     key.NewBinding(key.WithKeys("ctrl+b", "pgup"), key.WithHelp("ctrl+b", "page up")),
		PageDown:   key.NewBinding(key.WithKeys("ctrl+f", "pgdown"), key.WithHelp("ctrl+f", "page down")),
		Open:       key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
		Back:       key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		LineDown:   key.NewBinding(key.WithKeys("J"), key.WithHelp("J/K", "scroll one line")),
		LineUp:     key.NewBinding(key.WithKeys("K"), key.WithHelp("J/K", "scroll one line")),
		Expand:     key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "expand / collapse a thread")),
		Quit:       key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "back / quit")),
		Mail:       key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "mail")),
		Calendar:   key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "calendar")),
		Switch:     key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch screen")),
		Search:     key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Archive:    key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "archive")),
		Trash:      key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "trash")),
		ToggleRead: key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "read / unread")),
		Star:       key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "star")),
		Account:    key.NewBinding(key.WithKeys("A"), key.WithHelp("A", "account filter")),
		Mailbox:    key.NewBinding(key.WithKeys("M"), key.WithHelp("M", "mailbox")),
		Refresh:    key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh")),
		Undo:       key.NewBinding(key.WithKeys("z"), key.WithHelp("z", "undo")),
		Help:       key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	}
}

// helpLines is the ? overlay, built from the bindings above.
func (k keymap) helpLines() [][2]string {
	return [][2]string{
		{"j / k  ↓ ↑", "move"},
		{"g / G", "top / bottom"},
		{"ctrl+f / ctrl+b", "page down / up"},
		{"enter", "open"},
		{"J / K", "scroll one line (an expanded thread)"},
		{"t", "thread: expanded text / one row per message"},
		{"esc / q", "back (quit at the top)"},
		{"tab, 1, 2", "switch mail / calendar"},
		{"/", "search (FTS5 syntax)"},
		{"", ""},
		{"e", "archive"},
		{"d", "trash"},
		{"m", "toggle read / unread"},
		{"s", "toggle star"},
		{"z", "undo the last action"},
		{"", ""},
		{"A", "cycle the account filter"},
		{"M", "cycle the mailbox (inbox / all / …)"},
		{"R", "refresh now (and nudge the sync daemon)"},
		{"?", "this help"},
		{"", ""},
		{"r", "reply — not implemented yet"},
	}
}
