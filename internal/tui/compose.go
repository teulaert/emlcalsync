package tui

import (
	"errors"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/compose"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// composeView is the reply editor: four header fields over a body, filled in
// from the message being answered.
//
// The quoted original goes into the body up front rather than being appended
// at send time the way `emlcal mail reply` does it. On the command line the
// quote is invisible either way; here the whole point of an editor is that
// what is on screen is what goes out, and trimming the quote down to the line
// being answered is the normal thing to write.
//
// It is the only screen that captures every key. A letter typed into a header
// field must not archive the message underneath, so the root asks
// capturingKeys() before matching any of its own bindings, and this screen
// handles esc itself. The one key it never sees is ctrl+c: the root takes that
// as quit ahead of the gate, so nothing typed here can hold the program.
type composeView struct {
	d Deps

	// kind is what this composer was opened as. It decides the title, what the
	// status line calls a submission, and -- with threadID -- whether there is
	// a conversation behind the screen for ctrl+g to read at all.
	kind composeKind

	// account is the one the message goes out from: the account that received
	// what is being answered, or the one the draft is stored in. `mail reply`
	// follows the same rule, and there is no --from here to override it.
	account string
	from    model.Address

	// orig is the message being answered, kept so a reply that really goes out
	// can mark it answered. It is nil on a draft being finished: the draft
	// carries the threading headers, but not which message they point at.
	orig *model.Message

	// draftRemote is the stored draft this composer is finishing, empty when
	// it is a fresh reply. Neither provider can update a draft in place, so
	// saving or sending one creates the new message and trashes this one.
	draftRemote string
	// threadID is the conversation a send belongs to.
	threadID string

	// inReplyTo and references are the threading headers, worked out when the
	// composer opened. The header fields are editable; these are not. They are
	// what puts the message in the conversation.
	inReplyTo  string
	references []string

	fields []*textinput.Model
	to     textinput.Model
	cc     textinput.Model
	bcc    textinput.Model
	subj   textinput.Model
	body   textarea.Model

	// seed is what every field held when the composer opened, so esc can tell
	// an untouched composer from one holding work.
	seed composeSeed

	// focus indexes fields, or is bodyFocus for the body.
	focus int

	// files are the attachments going out with the message: a forward's, and
	// nothing else's. filesNote names any the fetch could not get, and stays
	// on screen rather than in a status that the next keystroke clears.
	files     []mime.DraftAttachment
	filesNote string

	// quote is the quoted original the reply opened with, so the AI draft
	// can tell the person's own text from what sits under it. Empty on a
	// stored draft, whose quote is wherever the person left it.
	quote string

	// asking is the AI instructions prompt, open on the status line; instr
	// is what has been typed into it.
	asking bool
	instr  string
	// assist is the AI draft arriving, nil when none is. assistSeq names the
	// generation, so a stopped one's stragglers are told from the current.
	assist    *assistState
	assistSeq int

	sending bool
	// pending is the two-press action waiting on its second press, so throwing
	// work away is never one keystroke.
	pending pendingAction
	err     error
	// info is a plain status the composer wants shown, cleared by the next
	// key the way err is.
	info string

	sized [2]int // the w, h the fields were last laid out for
}

// composeKind is the four things a composer can be: an answer, an answer being
// finished, a message passed on, and one with nothing behind it at all.
type composeKind int

const (
	kindReply composeKind = iota
	kindDraft
	kindForward
	kindNew
)

const (
	// bodyFocus is the focus index of the body, one past the header fields.
	bodyFocus = 4
	// labelW is the width of the "To" / "Subject" gutter.
	labelW = 9
)

var composeLabels = [bodyFocus]string{"To", "Cc", "Bcc", "Subject"}

// pendingAction is an action that has been asked for once and is waiting to be
// asked for again.
type pendingAction int

const (
	pendingNone pendingAction = iota
	// pendingDiscard: esc, on a composer with work in it.
	pendingDiscard
	// pendingDelete: ctrl+x, which takes the stored draft with it.
	pendingDelete
	// pendingReplace: ctrl+g, when there is text above the quote that the
	// AI draft would replace.
	pendingReplace
)

var (
	// errNoRecipient is the one thing the composer refuses to submit.
	errNoRecipient = errors.New("nobody to send this to: fill in To")
	// errNoStoredDraft is ctrl+x on a reply that was never saved.
	errNoStoredDraft = errors.New("nothing stored to delete — esc abandons this reply")
)

// composeSeed is every field as the composer opened with it.
type composeSeed struct{ to, cc, bcc, subj, body string }

// newReplyCompose opens a reply to orig.
func newReplyCompose(d Deps, orig *model.Message, all bool) *composeView {
	from := d.sendFrom(orig.AccountID)

	// The header half comes from the shared composer, so what this screen
	// opens with is the reply `emlcal mail reply` would have built.
	seed := &mime.Draft{From: from}
	compose.Reply(seed, orig, all, []string{from.Email})

	c := newComposeView(d, kindReply, orig.AccountID, from)
	c.orig = orig
	c.threadID = orig.ThreadID
	c.inReplyTo, c.references = seed.InReplyTo, seed.References
	c.quote = compose.Quote(orig, d.loc())
	c.fill(
		compose.JoinAddresses(seed.To),
		compose.JoinAddresses(seed.Cc),
		"",
		seed.Subject,
		// Two blank lines above the quote, with the cursor on the first: a
		// reply is written at the top, over what it answers.
		"\n\n"+c.quote,
	)
	return c
}

// newDraftCompose reopens a stored draft to finish it.
//
// The draft's text goes in whole, quote and all: it is a message someone was
// part way through writing, not one to be read, so nothing about it is
// stripped or rebuilt. What it does not carry is the message it answers --
// only the headers pointing at it -- so sending it marks nothing answered,
// exactly as `mail send --draft` does not.
func newDraftCompose(d Deps, m *model.Message) *composeView {
	from := m.From
	if from.Email == "" {
		from = d.sendFrom(m.AccountID)
	}
	c := newComposeView(d, kindDraft, m.AccountID, from)
	c.draftRemote = m.RemoteID
	c.threadID = m.ThreadID
	c.inReplyTo = m.InReplyTo
	c.references = append([]string(nil), m.References...)
	c.fill(
		compose.JoinAddresses(m.To),
		compose.JoinAddresses(m.Cc),
		compose.JoinAddresses(m.Bcc),
		m.Subject,
		m.TextBody,
	)
	return c
}

// newForwardCompose passes orig on to somebody else.
//
// A forward is not a reply and is not built like one. There are no recipients
// to work out -- who it goes to is the whole of what the person has to say, so
// the cursor opens in To rather than in the body -- and it carries no
// threading headers: In-Reply-To would file the message at the other end under
// a conversation the recipient has never seen a word of. Nor does it mark the
// original answered, because it does not answer it.
//
// What goes into the body is [compose.Forwarded]: the original entire, the
// rounds before the last one included and none of it marked "> ". A reply
// strips those because the person being answered wrote them; the whole point
// of a forward is that this person has none of it.
//
// The files come with it. They are fetched with the message rather than at
// send time -- the composer shows what is going out, and a row of attachments
// nobody can see until afterwards is the same as not having them -- and any
// that could not be fetched are named in that row rather than dropped.
func newForwardCompose(d Deps, orig *model.Message, files []mime.DraftAttachment, note string) *composeView {
	c := newComposeView(d, kindForward, orig.AccountID, d.sendFrom(orig.AccountID))
	c.files, c.filesNote = files, note
	// Two blank lines above the forwarded message, with the cursor waiting in
	// To: a note over somebody else's mail, addressed first.
	c.fill("", "", "", compose.ForwardSubject(orig.Subject),
		"\n\n"+compose.Forwarded(orig, d.loc()))
	c.focusField(0)
	return c
}

// newBlankCompose opens a message with nothing behind it: c.
//
// Every other composer takes its account from the message it opened on. This
// one has no message, so the caller decides -- the list's account filter, in
// practice -- and the status line names the address it will go out as, because
// a sender nobody picked on screen is the one thing about a new message that
// is not written anywhere on it.
func newBlankCompose(d Deps, account string, from model.Address) *composeView {
	c := newComposeView(d, kindNew, account, from)
	c.fill("", "", "", "", "")
	c.focusField(0)
	if from.Email != "" {
		c.info = "from " + from.Email
	}
	return c
}

func newComposeView(d Deps, kind composeKind, account string, from model.Address) *composeView {
	c := &composeView{d: d, kind: kind, account: account, from: from, focus: bodyFocus}
	c.to, c.cc, c.bcc, c.subj = newField(), newField(), newField(), newField()
	c.fields = []*textinput.Model{&c.to, &c.cc, &c.bcc, &c.subj}

	c.body = textarea.New()
	c.body.Prompt = ""
	c.body.ShowLineNumbers = false
	c.body.MaxHeight = 0 // a tall terminal is not a reason to clip the editor
	c.body.SetStyles(plainAreaStyles())
	// ctrl+d is send here, so it cannot also be delete-forward. `delete`
	// still is.
	c.body.KeyMap.DeleteCharacterForward = key.NewBinding(key.WithKeys("delete"))
	c.body.Focus()
	return c
}

// fill puts the opening contents in and remembers them, so esc knows later
// whether anything has been touched.
func (c *composeView) fill(to, cc, bcc, subj, body string) {
	c.seed = composeSeed{to: to, cc: cc, bcc: bcc, subj: subj, body: body}
	for i, v := range []string{to, cc, bcc, subj} {
		c.fields[i].SetValue(v)
		c.fields[i].CursorEnd()
	}
	c.body.SetValue(body)
	c.body.MoveToBegin()
}

func newField() textinput.Model {
	t := textinput.New()
	t.Prompt = ""
	t.SetStyles(plainFieldStyles())
	t.KeyMap.DeleteCharacterForward = key.NewBinding(key.WithKeys("delete"))
	return t
}

// plainFieldStyles and plainAreaStyles strip the bubbles' own palette. The
// rest of the TUI paints with the terminal's own attributes only (render.go),
// and a field arriving in fixed 256-colour grey is the one thing on screen
// that would not read on both a light and a dark background.
func plainFieldStyles() textinput.Styles {
	var s textinput.Styles
	plain := textinput.StyleState{Placeholder: styleFaint, Suggestion: styleFaint}
	s.Focused, s.Blurred = plain, plain
	s.Cursor.Blink = false
	return s
}

func plainAreaStyles() textarea.Styles {
	var s textarea.Styles
	plain := textarea.StyleState{Placeholder: styleFaint, EndOfBuffer: styleFaint}
	s.Focused, s.Blurred = plain, plain
	s.Cursor.Blink = false
	return s
}

func (c *composeView) Title() string {
	s := strings.TrimSpace(c.subj.Value())
	if s == "" {
		s = "(no subject)"
	}
	return c.kindWord() + " · " + s
}

// kindWord names the composer on the title bar. A stored draft says so
// whatever it started life as: what is on screen then is the person's own
// words from before, which is the thing worth being told.
func (c *composeView) kindWord() string {
	switch {
	case c.draftRemote != "":
		return "draft"
	case c.kind == kindForward:
		return "fwd"
	case c.kind == kindNew:
		return "new"
	}
	return "reply"
}

// Init has nothing to load: everything the composer shows was in hand when it
// was pushed.
func (c *composeView) Init() tea.Cmd { return nil }

// reload is a no-op. The root re-queries the visible screen every time the
// daemon commits, and re-reading the archive under a half-written reply would
// throw the text away.
func (c *composeView) reload() tea.Cmd { return nil }

// targets is empty: archiving or starring "the composer" means nothing, and
// the keys that would do it are being typed into it anyway.
func (c *composeView) targets() []target { return nil }

func (c *composeView) capturingKeys() bool { return true }

func (c *composeView) Update(msg tea.Msg, k keymap, w, h int) (screen, tea.Cmd) {
	c.ensure(w, h)
	if ev, ok := msg.(modelEvent); ok {
		return c, c.onDraft(ev)
	}
	press, isKey := msg.(tea.KeyPressMsg)
	if !isKey {
		return c, c.toFocused(msg)
	}
	if c.sending {
		return c, nil // the submission is in flight; nothing to type into
	}
	if c.assist != nil {
		// A draft is arriving. esc stops it; nothing else is taken, since
		// typing into text that is still being written would tangle the two.
		if press.String() == "esc" {
			c.stopAssist()
		}
		return c, nil
	}
	if c.asking {
		return c, c.askKey(press)
	}

	switch {
	// ctrl+c never reaches here: the root takes it as quit, ahead of the
	// capture gate, so a composer cannot hold the program.
	//
	// esc is matched on the key rather than through k.Back, which also answers
	// to u: in here that is a letter somebody is typing into a To field.
	case press.String() == "esc":
		return c, c.cancel()
	case key.Matches(press, k.Send):
		return c, c.submit(sync.OpSend)
	case key.Matches(press, k.SaveDraft):
		return c, c.submit(sync.OpDraft)
	case key.Matches(press, k.DeleteDraft):
		return c, c.delete()
	case key.Matches(press, k.AI):
		return c, c.startAsk()
	case key.Matches(press, k.NextField):
		c.moveFocus(1)
		return c, nil
	case key.Matches(press, k.PrevField):
		c.moveFocus(-1)
		return c, nil
	}

	// Anything else is text. A keystroke that reaches the buffer is a change
	// of mind about leaving, so a pending question lapses.
	c.pending, c.err, c.info = pendingNone, nil, ""
	return c, c.toFocused(msg)
}

// toFocused hands a message to whichever field has the cursor. Only that one
// gets it: a blurred input ignores key presses anyway.
func (c *composeView) toFocused(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	if c.focus == bodyFocus {
		c.body, cmd = c.body.Update(msg)
		return cmd
	}
	f := c.fields[c.focus]
	*f, cmd = f.Update(msg)
	return cmd
}

func (c *composeView) moveFocus(d int) {
	n := bodyFocus + 1
	c.focusField(((c.focus+d)%n + n) % n)
}

// focusField puts the cursor in one field. A reply opens in the body, over the
// quote it answers; a forward and a new message open in To, because they are
// addressed before they are written.
func (c *composeView) focusField(i int) {
	c.focus = i
	for j, f := range c.fields {
		if j == i {
			f.Focus()
		} else {
			f.Blur()
		}
	}
	if i == bodyFocus {
		c.body.Focus()
	} else {
		c.body.Blur()
	}
}

// cancel closes the composer, asking first when there is work to lose. A
// stored draft is left exactly as it was: cancelling is not deleting.
func (c *composeView) cancel() tea.Cmd {
	if !c.edited() || c.pending == pendingDiscard {
		return closeScreen()
	}
	c.pending = pendingDiscard
	return nil
}

// delete throws the stored draft away, asking first. There is nothing to
// delete on a reply that has never been saved -- esc is what abandons that --
// so the key says so rather than quietly doing nothing.
func (c *composeView) delete() tea.Cmd {
	if c.draftRemote == "" {
		c.err = errNoStoredDraft
		return nil
	}
	if c.pending != pendingDelete {
		c.pending = pendingDelete
		return nil
	}
	c.sending, c.err, c.pending = true, nil, pendingNone
	return c.d.submit("delete", c.account,
		sync.Op{Kind: sync.OpTrash, IDs: []string{c.draftRemote}}, nil, "")
}

// edited reports whether anything has been changed. The composer opens holding
// the quoted original and the recipients it worked out -- or the whole stored
// draft -- so comparing against what it opened with is what tells work from an
// untouched screen, and an untouched screen closes on the first esc.
func (c *composeView) edited() bool {
	return c.seed != composeSeed{
		to:   c.to.Value(),
		cc:   c.cc.Value(),
		bcc:  c.bcc.Value(),
		subj: c.subj.Value(),
		body: c.body.Value(),
	}
}

// submit builds the message and hands it to the engine. A message that will
// not build comes back as a failed submission rather than being handled here,
// so there is one path -- and one status line -- for "this did not go out".
func (c *composeView) submit(kind sync.OpKind) tea.Cmd {
	what, orig := c.what(kind), c.orig
	if kind == sync.OpDraft {
		// Nothing has gone out, so nothing has been answered.
		orig = nil
	}
	op, err := c.build(kind)
	if err != nil {
		return func() tea.Msg { return submitted{what: what, err: err} }
	}
	c.sending, c.err = true, nil
	// A draft cannot be updated in place, so what replaces it is trashed once
	// the new one is really there -- and only then, or a failed save would
	// take the work with it.
	return c.d.submit(what, c.account, op, orig, c.draftRemote)
}

// what names the action for the status line: finishing a stored draft is a
// send, not a reply, because the composer no longer knows what it answers.
func (c *composeView) what(kind sync.OpKind) string {
	switch {
	case kind == sync.OpDraft:
		return "draft"
	case c.draftRemote != "", c.kind == kindNew:
		return "send"
	case c.kind == kindForward:
		return "forward"
	}
	return "reply"
}

// build turns the fields into the op the engine takes. The threading headers
// are the composer's, not the fields': what puts a message in a conversation
// is not something to be typed over.
func (c *composeView) build(kind sync.OpKind) (sync.Op, error) {
	to, err := compose.ParseAddressList([]string{c.to.Value()})
	if err != nil {
		return sync.Op{}, err
	}
	cc, err := compose.ParseAddressList([]string{c.cc.Value()})
	if err != nil {
		return sync.Op{}, err
	}
	bcc, err := compose.ParseAddressList([]string{c.bcc.Value()})
	if err != nil {
		return sync.Op{}, err
	}
	if len(to)+len(cc)+len(bcc) == 0 {
		return sync.Op{}, errNoRecipient
	}
	draft := &mime.Draft{
		From:        c.from,
		To:          to,
		Cc:          cc,
		Bcc:         bcc,
		Subject:     strings.TrimSpace(c.subj.Value()),
		TextBody:    c.body.Value(),
		InReplyTo:   c.inReplyTo,
		References:  c.references,
		Attachments: c.files,
		Date:        c.d.now(),
	}
	raw, err := mime.Build(draft)
	if err != nil {
		return sync.Op{}, err
	}
	op := sync.Op{Kind: kind, Raw: raw}
	if kind == sync.OpSend {
		op.ThreadID = c.threadID
		op.From = draft.From.Email
		op.Recipients = compose.Envelope(draft)
	}
	return op, nil
}

// headerRows is how many rows sit above the rule: the four fields, and the
// files row when there is one. The body gets what is left.
func (c *composeView) headerRows() int {
	if len(c.files) > 0 || c.filesNote != "" {
		return len(c.fields) + 1
	}
	return len(c.fields)
}

// filesView is the attachments row: what is going out with the message, and
// what could not be got.
func (c *composeView) filesView(w int) string {
	names := make([]string, 0, len(c.files)+1)
	for _, f := range c.files {
		names = append(names, f.Filename+" "+output.HumanSize(int64(len(f.Data))))
	}
	line := strings.Join(names, ", ")
	if c.filesNote != "" {
		if line != "" {
			line += " · "
		}
		line += c.filesNote
	}
	return padCells(line, max(w-labelW, 0))
}

// ensure lays the fields out for the window the root is handing down.
func (c *composeView) ensure(w, h int) {
	if c.sized == [2]int{w, h} || w <= 0 || h <= 0 {
		return
	}
	c.sized = [2]int{w, h}
	fw := max(w-labelW-1, 1)
	for _, f := range c.fields {
		f.SetWidth(fw)
	}
	c.body.SetWidth(max(w-2, 1))
	c.body.SetHeight(max(listRows(h)-c.headerRows()-1, 1))
}

func (c *composeView) View(w, h int) string {
	c.ensure(w, h)
	rows := listRows(h)
	out := make([]string, 0, rows)
	for i, f := range c.fields {
		label := padCells(" "+composeLabels[i], labelW)
		if i != c.focus {
			label = styleFaint.Render(label)
		}
		out = append(out, label+f.View())
	}
	if c.headerRows() > len(c.fields) {
		out = append(out, styleFaint.Render(padCells(" Files", labelW))+c.filesView(w))
	}
	out = append(out, styleFaint.Render(strings.Repeat("─", max(w, 0))))
	for _, l := range strings.Split(c.body.View(), "\n") {
		out = append(out, " "+l)
	}
	for len(out) < rows {
		out = append(out, "")
	}
	return strings.Join(out[:rows], "\n")
}

func (c *composeView) footer(w int) string {
	switch {
	case c.sending:
		return "sending…"
	case c.assist != nil:
		return c.assistFooter()
	case c.asking:
		return padCells("ai · instructions, or enter alone to just answer it: "+c.instr+"█", w)
	case c.err != nil:
		return styleErr.Render(c.err.Error())
	case c.pending == pendingDelete:
		return styleErr.Render("ctrl+x again to delete this draft (it goes to the trash)")
	case c.pending == pendingReplace:
		return styleErr.Render("the AI draft replaces what you wrote above the quote — ctrl+g again to go on")
	case c.pending == pendingDiscard && c.draftRemote != "":
		return "esc again to leave the draft as it was"
	case c.pending == pendingDiscard:
		return "esc again to throw this reply away"
	case c.info != "":
		return c.info
	}
	// The hints are what this composer can actually do: there is nothing to
	// delete without a stored draft, and nothing for the model to read without
	// a conversation behind the screen.
	hints := []string{"ctrl+d send"}
	if c.draftRemote != "" {
		hints = append(hints, "ctrl+s save", "ctrl+x delete")
	} else {
		hints = append(hints, "ctrl+s save as draft")
	}
	if c.threadID != "" {
		hints = append(hints, "ctrl+g ai draft")
	}
	return strings.Join(append(hints, "tab / shift+tab field", "esc cancel"), " · ")
}

var (
	_ screen    = (*composeView)(nil)
	_ capturing = (*composeView)(nil)
)
