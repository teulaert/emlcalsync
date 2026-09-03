package tui

import (
	"strings"
	"testing"
)

// openNew opens a fresh message on an archive that knows Anna, Bob and
// Carol (addConversation), with the cursor in To.
func openNew(t *testing.T) (*root, *composeView) {
	t.Helper()
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	r := newTestRoot(t, d)
	send(t, r, "c")
	c := composerOn(t, r)
	if c.focus != 0 {
		t.Fatalf("a new message opens in To, not field %d", c.focus)
	}
	if len(c.book) == 0 {
		t.Fatal("the composer opened without the address book")
	}
	return r, c
}

func typeText(t *testing.T, r *root, s string) {
	t.Helper()
	for _, ch := range s {
		send(t, r, string(ch))
	}
}

func TestTypingInToOffersAddressesFromTheArchive(t *testing.T) {
	r, c := openNew(t)
	typeText(t, r, "vri")
	if len(c.hints) != 1 || c.hints[0].Email != "anna@example.com" {
		t.Fatalf("hints = %+v, want Anna de Vries by part of her surname", c.hints)
	}
	if foot := c.footer(100); !strings.Contains(foot, "Anna de Vries <anna@example.com>") || !strings.HasPrefix(foot, "enter takes") {
		t.Errorf("footer = %q", foot)
	}
	typeText(t, r, "x")
	if len(c.hints) != 0 {
		t.Errorf("hints = %+v after typing past every match", c.hints)
	}
	if !strings.Contains(c.footer(100), "ctrl+d send") {
		t.Errorf("footer = %q, want the usual hints back", c.footer(100))
	}
}

func TestEnterTakesTheSuggestedAddressAndKeepsTheOthers(t *testing.T) {
	r, c := openNew(t)
	c.to.SetValue("bob@example.com,")
	c.to.CursorEnd()
	typeText(t, r, " an")
	if len(c.hints) != 1 || c.hints[0].Email != "anna@example.com" {
		t.Fatalf("hints = %+v", c.hints)
	}
	send(t, r, "enter")
	if got := c.to.Value(); got != "bob@example.com, Anna de Vries <anna@example.com>, " {
		t.Errorf("to = %q", got)
	}
	if len(c.hints) != 0 {
		t.Errorf("hints linger after being taken: %+v", c.hints)
	}
	// What is on the line already is not offered again.
	typeText(t, r, "example")
	for _, h := range c.hints {
		if h.Email == "anna@example.com" || h.Email == "bob@example.com" {
			t.Errorf("%s is offered although it is on the line", h.Email)
		}
	}
}

func TestCtrlNCyclesTheSuggestions(t *testing.T) {
	r, c := openNew(t)
	typeText(t, r, "example")
	if len(c.hints) < 3 {
		t.Fatalf("hints = %+v, want everyone at example.com", c.hints)
	}
	send(t, r, "ctrl+n")
	if c.hint != 1 {
		t.Errorf("after ctrl+n hint = %d", c.hint)
	}
	send(t, r, "ctrl+p")
	send(t, r, "ctrl+p")
	if c.hint != len(c.hints)-1 {
		t.Errorf("ctrl+p wraps to the end: hint = %d", c.hint)
	}
	want := c.hints[c.hint].Email
	send(t, r, "enter")
	if !strings.Contains(c.to.Value(), want) {
		t.Errorf("to = %q, want the selected %s", c.to.Value(), want)
	}
	// Typing again starts the selection over.
	typeText(t, r, "e")
	if c.hint != 0 {
		t.Errorf("hint = %d after typing", c.hint)
	}
}

func TestSuggestionsStayOutOfTheSubjectAndTheBody(t *testing.T) {
	r, c := openNew(t)
	typeText(t, r, "an")
	if len(c.hints) == 0 {
		t.Fatal("no hints in To")
	}
	send(t, r, "tab") // Cc: leaving the field drops the offer
	if len(c.hints) != 0 {
		t.Errorf("hints follow the cursor out of To: %+v", c.hints)
	}
	send(t, r, "tab")
	send(t, r, "tab") // Subject
	typeText(t, r, "an")
	if len(c.hints) != 0 {
		t.Errorf("the subject is completed: %+v", c.hints)
	}
	send(t, r, "tab") // body
	typeText(t, r, "an")
	send(t, r, "enter")
	if len(c.hints) != 0 || !strings.Contains(c.body.Value(), "an\n") {
		t.Errorf("body = %q, hints = %+v; enter in the body is a newline", c.body.Value(), c.hints)
	}
}

func TestEnterWithNothingSuggestedIsJustEnter(t *testing.T) {
	r, c := openNew(t)
	typeText(t, r, "zzz")
	send(t, r, "enter")
	if c.to.Value() != "zzz" || c.focus != 0 {
		t.Errorf("to = %q, focus = %d", c.to.Value(), c.focus)
	}
}

func TestTheComposerOpensWithoutAnAddressBook(t *testing.T) {
	d := newTestDeps(t, "work")
	r := newTestRoot(t, d)
	send(t, r, "c")
	c := composerOn(t, r)
	typeText(t, r, "an")
	if len(c.hints) != 0 || c.to.Value() != "an" {
		t.Errorf("to = %q, hints = %+v", c.to.Value(), c.hints)
	}
}

func TestSplitTail(t *testing.T) {
	for in, want := range map[string][2]string{
		"":                        {"", ""},
		"an":                      {"", "an"},
		"a@x, b":                  {"a@x, ", "b"},
		"a@x,b":                   {"a@x, ", "b"},
		"a@x, ":                   {"a@x, ", ""},
		`"Vries, Anna" <a@x>, bo`: {`"Vries, Anna" <a@x>, `, "bo"},
		`"Vries, An`:              {"", `"Vries, An`},
	} {
		head, tail := splitTail(in)
		if head != want[0] || tail != want[1] {
			t.Errorf("splitTail(%q) = %q, %q; want %q, %q", in, head, tail, want[0], want[1])
		}
	}
}

func TestHintsFooterFitsTheWidth(t *testing.T) {
	r, c := openNew(t)
	typeText(t, r, "example")
	if n := len(c.hints); n < 3 {
		t.Fatalf("hints = %d", n)
	}
	// Room for the lead and one address: the selected one is shown on its
	// own, whichever it is.
	c.hint = len(c.hints) - 1
	foot := c.footer(50)
	last := c.hints[c.hint].Email
	if !strings.Contains(foot, last) || strings.Contains(foot, c.hints[0].Email) {
		t.Errorf("footer(50) = %q, want just %s", foot, last)
	}
	if strings.Count(c.footer(200), " · ") != len(c.hints) {
		t.Errorf("footer(200) = %q, want every hint", c.footer(200))
	}
}
