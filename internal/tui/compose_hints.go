package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-runewidth"

	"github.com/teulaert/emlcalsync/internal/compose"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/store"
)

// The composer completes To, Cc and Bcc out of the address book the archive
// derives (store.SearchContacts). The book is loaded once, when the composer
// opens, and matched in memory on every keystroke, so typing never waits on a
// query. The bubbles' own suggestions are not used: they complete the whole
// field by prefix, and a To field is a list whose last item is the one being
// typed, matched anywhere in a name or an address.

const (
	// contactBookLimit is how much of the book one composer holds. The book
	// is ranked, so what is cut off is whoever wrote in once, long ago.
	contactBookLimit = 2000
	// maxHints is how many matches are offered at once.
	maxHints = 5
)

// contactsLoaded carries the address book to the composer that asked.
type contactsLoaded struct {
	contacts []model.Contact
	err      error
}

func (d Deps) loadContacts() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cs, err := d.Store.SearchContacts(ctx, store.ContactFilter{Limit: contactBookLimit})
		return contactsLoaded{contacts: cs, err: err}
	}
}

// hintable reports whether the cursor is in a field the book completes.
func (c *composeView) hintable() bool { return c.focus >= 0 && c.focus < 3 }

// splitTail divides a header field into the addresses already there and the
// one being typed: everything up to the last comma outside quotes, and what
// follows it. The head comes back ending in ", " (or empty), so that
// head + address + ", " is the field with the tail replaced.
func splitTail(s string) (head, tail string) {
	inQuote := false
	cut := -1
	for i, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ',' && !inQuote:
			cut = i
		}
	}
	if cut < 0 {
		return "", strings.TrimSpace(s)
	}
	return strings.TrimSpace(s[:cut+1]) + " ", strings.TrimSpace(s[cut+1:])
}

// refreshHints matches what is being typed against the book. It runs after
// every keystroke into a header field; the selection starts over, since the
// list under it has changed.
func (c *composeView) refreshHints() {
	c.hints, c.hint = nil, 0
	if !c.hintable() || len(c.book) == 0 {
		return
	}
	head, tail := splitTail(c.fields[c.focus].Value())
	if tail == "" {
		return
	}
	q := strings.ToLower(tail)
	taken := map[string]bool{}
	for _, a := range compose.SplitAddresses(head) {
		if p, err := compose.ParseAddress(a); err == nil {
			taken[strings.ToLower(p.Email)] = true
		}
	}
	for i := range c.book {
		ct := &c.book[i]
		if taken[ct.Email] {
			continue
		}
		if !strings.Contains(strings.ToLower(ct.Name), q) && !strings.Contains(ct.Email, q) {
			continue
		}
		// Typed out in full already: nothing left to offer.
		if strings.EqualFold(compose.FormatAddress(ct.Address()), tail) {
			continue
		}
		c.hints = append(c.hints, *ct)
		if len(c.hints) == maxHints {
			break
		}
	}
}

// takeHint replaces what is being typed with the selected address, and
// leaves the cursor after a separator, ready for the next one. SplitAddresses
// drops the trailing separator when the message is built.
func (c *composeView) takeHint() {
	if len(c.hints) == 0 || !c.hintable() {
		return
	}
	f := c.fields[c.focus]
	head, _ := splitTail(f.Value())
	f.SetValue(head + compose.FormatAddress(c.hints[c.hint].Address()) + ", ")
	f.CursorEnd()
	c.hints, c.hint = nil, 0
}

func (c *composeView) cycleHint(d int) {
	if n := len(c.hints); n > 0 {
		c.hint = ((c.hint+d)%n + n) % n
	}
}

// hintsFooter is the status line while there is something to offer: the
// matches, the selected one reversed, as many around it as fit.
func (c *composeView) hintsFooter(w int) string {
	const lead = "enter takes · "
	const sep = " · "
	room := w - runewidth.StringWidth(lead)
	strs := make([]string, len(c.hints))
	for i, h := range c.hints {
		strs[i] = compose.FormatAddress(h.Address())
	}
	strs[c.hint] = truncCells(strs[c.hint], max(room, 1))
	first, last := c.hint, c.hint
	used := runewidth.StringWidth(strs[c.hint])
	for {
		grown := false
		if last < len(strs)-1 && used+runewidth.StringWidth(sep+strs[last+1]) <= room {
			last++
			used += runewidth.StringWidth(sep + strs[last])
			grown = true
		}
		if first > 0 && used+runewidth.StringWidth(sep+strs[first-1]) <= room {
			first--
			used += runewidth.StringWidth(sep + strs[first])
			grown = true
		}
		if !grown {
			break
		}
	}
	parts := make([]string, 0, last-first+1)
	for i := first; i <= last; i++ {
		if i == c.hint {
			parts = append(parts, styleSelected.Render(strs[i]))
		} else {
			parts = append(parts, strs[i])
		}
	}
	return lead + strings.Join(parts, sep)
}
