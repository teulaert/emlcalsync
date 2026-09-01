package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/itip"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
)

// reader shows one message. The body goes through a viewport because wrapping
// and scrolling long mail is exactly the fiddly work worth borrowing.
type reader struct {
	d Deps

	accountID string
	remote    string

	msg  *model.Message
	body string
	// invite is the calendar card, when the message carries one.
	invite *readerInvite
	// atts are the message's parts, so the header names the files.
	atts []model.Attachment

	vp      viewport.Model
	ready   bool
	seq     int
	loading bool
	loadErr error
}

func newReader(d Deps, accountID, remote string) *reader {
	return &reader{d: d, accountID: accountID, remote: remote}
}

func (r *reader) Title() string {
	if r.msg == nil {
		return "message"
	}
	s := r.msg.Subject
	if strings.TrimSpace(s) == "" {
		s = "(no subject)"
	}
	return s
}

func (r *reader) Init() tea.Cmd { return r.reload() }

func (r *reader) reload() tea.Cmd {
	r.seq++
	r.loading = true
	return r.d.loadBody(r.seq, r.accountID, r.remote)
}

// show swaps the reader onto another message, keeping the screen where it is.
// Following a triage to the next message in the thread is the one thing that
// changes what an open reader shows; everything else pushes a new one.
func (r *reader) show(accountID, remote string) tea.Cmd {
	r.accountID, r.remote = accountID, remote
	r.msg, r.body, r.invite, r.atts, r.loadErr = nil, "", nil, nil, nil
	if r.ready {
		r.vp.SetContent("")
		r.vp.GotoTop()
	}
	return r.reload()
}

func (r *reader) targets() []target {
	if r.msg == nil {
		return nil
	}
	return []target{targetOf(r.msg)}
}

func (r *reader) Update(msg tea.Msg, k keymap, w, h int) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case bodyLoaded:
		if msg.seq != r.seq {
			return r, nil // a newer body is on its way; do not paint this one
		}
		r.loading = false
		r.loadErr = msg.err
		if msg.err == nil {
			r.msg = msg.msg
			r.body = msg.body
			r.invite = msg.invite
			r.atts = msg.attachments
			r.setContent(w, h)
		}
		return r, nil

	case tea.KeyPressMsg:
		r.ensure(w, h)
		var cmd tea.Cmd
		r.vp, cmd = r.vp.Update(msg)
		return r, cmd
	}
	return r, nil
}

func (r *reader) ensure(w, h int) {
	body := listRows(h)
	head := len(r.headerLines(w))
	vh := body - head - 1
	if vh < 1 {
		vh = 1
	}
	if !r.ready {
		r.vp = viewport.New(viewport.WithWidth(w), viewport.WithHeight(vh))
		r.vp.SoftWrap = true
		r.ready = true
		r.vp.SetContent(r.body)
		return
	}
	if r.vp.Width() != w || r.vp.Height() != vh {
		r.vp.SetWidth(w)
		r.vp.SetHeight(vh)
	}
}

func (r *reader) setContent(w, h int) {
	r.ensure(w, h)
	r.vp.SetContent(r.body)
	r.vp.GotoTop()
}

func (r *reader) headerLines(w int) []string {
	if r.msg == nil {
		return []string{""}
	}
	m := r.msg
	var to []string
	for _, a := range m.To {
		to = append(to, a.String())
	}
	// The id first: it is what anything else that reads the archive -- an
	// agent with the skill -- needs to be told, and y copies it.
	lines := []string{
		truncCells("Id:      "+m.PublicID(), w),
		truncCells("From:    "+m.From.String(), w),
	}
	if len(to) > 0 {
		lines = append(lines, truncCells("To:      "+strings.Join(to, ", "), w))
	}
	lines = append(lines,
		truncCells("Date:    "+m.Date.In(r.d.loc()).Format("Mon, 02 Jan 2006 15:04"), w),
		truncCells("Subject: "+m.Subject, w),
	)
	for i, d := range attachmentDescs(r.atts, m.HasAttachments) {
		// The label on the first line only, the rest hanging under it, the
		// way the other headers align.
		label := "Attach:  "
		if i > 0 {
			label = "         "
		}
		lines = append(lines, truncCells(label+d, w))
	}
	for i, l := range lines {
		lines[i] = styleFaint.Render(l)
	}
	if r.invite != nil {
		lines = append(lines, "")
		lines = append(lines, inviteCard(r.invite, r.d.loc(), w)...)
	}
	return lines
}

// maxAttachmentLines caps the header block. A message with more parts than
// this is rare and never worth eating the body with; the overflow line still
// says how many are missing.
const maxAttachmentLines = 4

// attachmentDescs names what a message actually carries, one string per line:
// "invoice.pdf · 57.3KB". A flag saying "yes, something is attached" answers
// the wrong question -- what you want to know before filing a forward is
// whether the invoice is on it, and that is a filename.
//
// Inline parts are counted rather than listed: the images an HTML mail embeds
// in its own body are not what anyone means by an attachment, and a
// newsletter carries a dozen. Counting them keeps the block honest about
// everything the message holds without letting the decoration crowd out the
// document.
//
// hasAttachments is what the index says, which is the fallback when the rows
// themselves could not be read -- better to repeat the flag than to draw a
// message with files as one without.
func attachmentDescs(atts []model.Attachment, hasAttachments bool) []string {
	var named []string
	inline := 0
	for _, a := range atts {
		if a.Inline {
			inline++
			continue
		}
		named = append(named, attachmentName(a)+" · "+output.HumanSize(a.Size))
	}
	out := named
	// Truncating to N plus a "+1 more" line costs the same height as simply
	// listing N+1, so the cap only bites once it actually saves a line.
	if len(named) > maxAttachmentLines+1 {
		out = append(named[:maxAttachmentLines:maxAttachmentLines],
			fmt.Sprintf("+%d more", len(named)-maxAttachmentLines))
	}
	if inline > 0 {
		out = append(out, fmt.Sprintf("+%d inline", inline))
	}
	if len(out) == 0 && hasAttachments {
		// The index says there are files but no rows name them: a message
		// indexed before its parts were recorded, or a read that failed. Say
		// what is known rather than nothing.
		return []string{"yes"}
	}
	return out
}

// attachmentName is what to call a part on screen. Most carry a filename;
// one that does not is still worth naming by its type, and a part with
// neither by where it sits, which is what `attachment get` takes.
func attachmentName(a model.Attachment) string {
	if s := strings.TrimSpace(a.Filename); s != "" {
		return s
	}
	if s := strings.TrimSpace(a.ContentType); s != "" {
		return s
	}
	return "part " + a.PartPath
}

// inviteCard lays out an invitation the way a mail client shows one: between
// the headers and the text, what and when and who, and last what to do about
// it. It is the part of the message that matters, so it is not faint; the
// closing line is. Nil yields nothing, so callers need not check.
func inviteCard(ri *readerInvite, loc *time.Location, w int) []string {
	if ri == nil {
		return nil
	}
	var lines []string
	for _, f := range ri.inv.Fields(loc) {
		lines = append(lines, truncCells(padCells(f.Key+":", 12)+f.Value, w))
	}
	switch {
	case ri.answerable() && ri.inv.NeedsAnswer():
		lines = append(lines, styleFaint.Render(truncCells(padCells("Answer:", 12)+"y accept · n decline · t tentative", w)))
	case ri.local != nil:
		lines = append(lines, styleFaint.Render(truncCells(padCells("Calendar:", 12)+ri.calName+" · y/n/t changes the answer", w)))
	case ri.inv.Method == itip.MethodRequest:
		lines = append(lines, styleFaint.Render(truncCells(padCells("Calendar:", 12)+"not on a synced calendar yet", w)))
	}
	lines = append(lines, "")
	return lines
}

// rsvp is the RSVP the user just pressed, if any -- the event view's keys,
// so an invitation is answered the same way wherever it is met.
func (r *reader) rsvp(k string) (model.Participation, bool) {
	if !r.invite.answerable() {
		return "", false
	}
	switch k {
	case "y":
		return model.PartAccepted, true
	case "n":
		return model.PartDeclined, true
	case "t":
		return model.PartTentative, true
	}
	return "", false
}

func (r *reader) View(w, h int) string {
	rows := listRows(h)
	if r.msg == nil {
		msg := "  loading…"
		if r.loadErr != nil {
			msg = "  " + r.loadErr.Error()
		} else if !r.loading {
			msg = "  (not found)"
		}
		return strings.Join(append([]string{msg}, make([]string, rows-1)...), "\n")
	}
	r.ensure(w, h)
	out := r.headerLines(w)
	out = append(out, "")
	out = append(out, strings.Split(r.vp.View(), "\n")...)
	for len(out) < rows {
		out = append(out, "")
	}
	return strings.Join(out[:rows], "\n")
}

func (r *reader) footer(w int) string {
	if r.msg == nil {
		return ""
	}
	f := output.MailFlags(r.msg.Flags, r.msg.HasAttachments)
	if f == "" {
		f = "-"
	}
	if r.invite.answerable() {
		return fmt.Sprintf("y accept · n decline · t tentative · %s · %d%%", f, int(r.vp.ScrollPercent()*100))
	}
	return fmt.Sprintf("%s · %s · %d%%", r.msg.PublicID(), f, int(r.vp.ScrollPercent()*100))
}
