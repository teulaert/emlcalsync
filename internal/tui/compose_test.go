package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// addConversation indexes one message with real recipients, which is what a
// reply-to-all has to have something to work from.
func addConversation(t *testing.T, d Deps, account, remote, thread string) {
	t.Helper()
	m := &model.Message{
		AccountID:       account,
		RemoteID:        remote,
		ThreadID:        thread,
		MessageIDHeader: "orig@example.com",
		References:      []string{"root@example.com"},
		Subject:         "offerte Q4",
		From:            model.Address{Name: "Anna de Vries", Email: "anna@example.com"},
		To:              []model.Address{{Email: account + "@example.com"}, {Email: "bob@example.com"}},
		Cc:              []model.Address{{Email: "carol@example.com"}},
		Date:            testNow.Add(-time.Hour),
		Received:        testNow.Add(-time.Hour),
		TextBody:        "Kun je dit bevestigen?",
		MailboxRemotes:  []string{"inbox"},
		IndexedAt:       testNow,
	}
	if _, err := d.Store.UpsertMessage(context.Background(), m, nil); err != nil {
		t.Fatalf("UpsertMessage %s: %v", remote, err)
	}
}

func composerOn(t *testing.T, r *root) *composeView {
	t.Helper()
	c, ok := r.top().(*composeView)
	if !ok {
		t.Fatalf("top = %T, want the composer", r.top())
	}
	return c
}

func TestReplyOpensTheComposerFilledIn(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")

	c := composerOn(t, r)
	if c.to.Value() != "Anna de Vries <anna@example.com>" {
		t.Errorf("to = %q, want the sender", c.to.Value())
	}
	if c.cc.Value() != "" {
		t.Errorf("cc = %q, want none on a plain reply", c.cc.Value())
	}
	if c.subj.Value() != "Re: offerte Q4" {
		t.Errorf("subject = %q", c.subj.Value())
	}
	if !strings.Contains(c.body.Value(), "> Kun je dit bevestigen?") {
		t.Errorf("body does not quote the original:\n%s", c.body.Value())
	}
	if !strings.HasPrefix(c.body.Value(), "\n\n") {
		t.Errorf("body does not open with room to write above the quote: %q", c.body.Value())
	}
	if c.focus != bodyFocus {
		t.Errorf("focus = %d, want the body: that is what you came to write", c.focus)
	}
	// The account that received it is the account it goes out from.
	if c.account != "work" || c.from.Email != "work@example.com" {
		t.Errorf("sending from %q as %q, want the account that received it", c.account, c.from.Email)
	}
}

func TestReplyAllKeepsTheOtherRecipients(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "a")

	c := composerOn(t, r)
	if !strings.Contains(c.to.Value(), "bob@example.com") {
		t.Errorf("to = %q, want the other recipient too", c.to.Value())
	}
	if strings.Contains(c.to.Value(), "work@example.com") {
		t.Errorf("to = %q, want my own address dropped", c.to.Value())
	}
	if c.cc.Value() != "carol@example.com" {
		t.Errorf("cc = %q, want carol", c.cc.Value())
	}
}

// A list row names a thread, not a message. With no answer under way the one
// to reply to is the newest message in it.
func TestReplyFromTheListAnswersTheNewestMessage(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	addMessage(t, d, "work", "w2", "t1", "Re: offerte Q4", "bob", time.Minute, false)

	r := newTestRoot(t, d)
	send(t, r, "r")

	c := composerOn(t, r)
	if c.draftRemote != "" {
		t.Errorf("opened a draft composer (%q) for a thread with no draft", c.draftRemote)
	}
	if got := c.orig.RemoteID; got != "w2" {
		t.Errorf("replying to %q, want the newest message w2", got)
	}
}

// Answering a conversation already half answered carries on with that answer
// rather than opening a second one beside it.
func TestReplyContinuesADraftAlreadyInTheThread(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	addDraft(t, d, "work", "w2", "t1", "Re: offerte Q4", 0)

	r := newTestRoot(t, d)
	send(t, r, "r")

	c := composerOn(t, r)
	if c.draftRemote != "w2" {
		t.Errorf("editing %q, want the draft already in the thread", c.draftRemote)
	}
	// The header is what says the words on screen are your own from earlier.
	if !strings.Contains(r.render(), "draft · ") {
		t.Errorf("the header does not say this is a draft:\n%s", r.render())
	}
}

// The same from inside the thread, where r is aimed at one message: what it
// finds is still the answer already under way.
func TestReplyInsideTheThreadContinuesTheDraft(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	addDraft(t, d, "work", "w2", "t1", "Re: offerte Q4", 0)

	r := newTestRoot(t, d)
	send(t, r, "enter")
	send(t, r, "j") // onto the message the draft answers
	send(t, r, "r")

	if got := composerOn(t, r).draftRemote; got != "w2" {
		t.Errorf("editing %q, want the draft w2", got)
	}
}

// Reply-all lands on the same draft: there is only ever one answer under way.
func TestReplyAllAlsoContinuesTheDraft(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	addDraft(t, d, "work", "w2", "t1", "Re: offerte Q4", 0)

	r := newTestRoot(t, d)
	send(t, r, "a")

	if got := composerOn(t, r).draftRemote; got != "w2" {
		t.Errorf("editing %q, want the draft w2", got)
	}
}

func TestReplyFromTheThreadAnswersTheSelectedMessage(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	addMessage(t, d, "work", "w2", "t1", "Re: offerte Q4", "bob", time.Minute, false)

	r := newTestRoot(t, d)
	send(t, r, "enter") // into the thread; the newest is on top
	send(t, r, "j")     // down to the older one
	send(t, r, "r")

	if got := composerOn(t, r).orig.RemoteID; got != "w1" {
		t.Errorf("replying to %q, want the message under the cursor", got)
	}
}

// What makes it a reply is the threading, and that has to survive into the
// bytes that go out.
func TestComposerBuildsAReplyInTheSameConversation(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	c.bcc.SetValue("hidden@example.com")

	op, err := c.build(sync.OpSend)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if op.Kind != sync.OpSend || op.ThreadID != "t1" {
		t.Errorf("op = %v thread %q, want a send in t1", op.Kind, op.ThreadID)
	}
	if op.From != "work@example.com" {
		t.Errorf("envelope from = %q", op.From)
	}
	if strings.Join(op.Recipients, ",") != "anna@example.com,hidden@example.com" {
		t.Errorf("envelope recipients = %v, want the Bcc carried alongside", op.Recipients)
	}

	parsed, err := mime.Parse(op.Raw)
	if err != nil {
		t.Fatalf("the composer built something unparseable: %v", err)
	}
	if parsed.InReplyTo != "orig@example.com" {
		t.Errorf("in-reply-to = %q", parsed.InReplyTo)
	}
	if len(parsed.References) != 2 || parsed.References[1] != "orig@example.com" {
		t.Errorf("references = %v", parsed.References)
	}
	if parsed.Subject != "Re: offerte Q4" {
		t.Errorf("subject = %q", parsed.Subject)
	}
	// Bcc must not reach the people who can read the message.
	if strings.Contains(string(op.Raw), "hidden@example.com") {
		t.Error("the Bcc address is in the message bytes")
	}
}

func TestComposerDraftCarriesNoEnvelope(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")

	op, err := composerOn(t, r).build(sync.OpDraft)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if op.Kind != sync.OpDraft {
		t.Errorf("kind = %v", op.Kind)
	}
	// A draft is stored, not submitted: there is no RCPT TO to fill in.
	if op.ThreadID != "" || op.From != "" || len(op.Recipients) != 0 {
		t.Errorf("draft carries a submission envelope: %+v", op)
	}
}

func TestComposerRefusesToSendWithNobodyToSendTo(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	c.to.SetValue("")

	send(t, r, "ctrl+d")
	if _, ok := r.top().(*composeView); !ok {
		t.Fatalf("the composer closed on a message it could not send (top = %T)", r.top())
	}
	if c.err == nil {
		t.Error("no error on the composer")
	}
	if c.sending {
		t.Error("the composer thinks a message it refused to build is in flight")
	}
	if !strings.Contains(r.status, "reply failed") {
		t.Errorf("status = %q, want it to say the reply did not go", r.status)
	}
}

func TestComposerRejectsAnAddressThatIsNotOne(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	c.to.SetValue("not an address")

	send(t, r, "ctrl+d")
	if c.err == nil || !strings.Contains(c.err.Error(), "invalid address") {
		t.Errorf("err = %v, want it to name the bad address", c.err)
	}
}

// esc on a reply someone has written asks first; on an untouched composer it
// just closes, because there is nothing to lose.
func TestEscapeAsksBeforeThrowingAwayAWrittenReply(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")
	send(t, r, "o")
	send(t, r, "k")

	send(t, r, "esc")
	c, ok := r.top().(*composeView)
	if !ok {
		t.Fatalf("one esc threw the reply away (top = %T)", r.top())
	}
	if c.pending != pendingDiscard {
		t.Error("the composer is not asking for a confirmation")
	}

	send(t, r, "esc")
	if _, ok := r.top().(*composeView); ok {
		t.Error("the second esc did not close the composer")
	}
}

func TestEscapeClosesAnUntouchedComposerAtOnce(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")
	send(t, r, "esc")

	if _, ok := r.top().(*composeView); ok {
		t.Error("an untouched composer asked before closing")
	}
}

// A keystroke after the question is a change of mind about leaving.
func TestTypingCancelsThePendingDiscard(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")
	send(t, r, "o")
	send(t, r, "esc")
	send(t, r, "k")
	send(t, r, "esc")

	c, ok := r.top().(*composeView)
	if !ok {
		t.Fatalf("the reply was thrown away without asking again (top = %T)", r.top())
	}
	if c.pending != pendingDiscard {
		t.Error("the second esc did not ask again")
	}
}

// The composer takes every key. "e" archives everywhere else in the mail
// stack; here it is a letter.
func TestComposerTakesTheKeysTheListWouldHaveActedOn(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")
	for _, k := range []string{"e", "d", "q", "?", "1", "2"} {
		send(t, r, k)
	}

	c, ok := r.top().(*composeView)
	if !ok {
		t.Fatalf("a letter left the composer (top = %T)", r.top())
	}
	if r.showHelp {
		t.Error("? opened the help overlay instead of being typed")
	}
	if r.onCal {
		t.Error("2 switched to the calendar instead of being typed")
	}
	if !strings.HasPrefix(c.body.Value(), "edq?12") {
		t.Errorf("body = %q, want the letters typed into it", c.body.Value())
	}
	// And the message underneath is untouched.
	m, err := d.Store.GetMessage(context.Background(), "work", "w1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if len(m.MailboxRemotes) != 1 || m.MailboxRemotes[0] != "inbox" {
		t.Errorf("the message moved to %v while a reply was being written", m.MailboxRemotes)
	}
}

func TestTabMovesBetweenTheFields(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)

	send(t, r, "tab") // body wraps round to To
	if c.focus != 0 || !c.to.Focused() {
		t.Errorf("focus = %d (to focused: %v), want To", c.focus, c.to.Focused())
	}
	send(t, r, "tab")
	if c.focus != 1 || !c.cc.Focused() {
		t.Errorf("focus = %d, want Cc", c.focus)
	}
	send(t, r, "shift+tab")
	if c.focus != 0 {
		t.Errorf("focus = %d, want back on To", c.focus)
	}

	send(t, r, "x")
	if !strings.HasSuffix(c.to.Value(), "x") {
		t.Errorf("to = %q, want the letter typed into the focused field", c.to.Value())
	}
	if c.body.Value() != c.seed.body {
		t.Error("the letter also reached the body")
	}
}

// Nothing on the calendar side has a message to answer.
func TestReplyDoesNothingOnTheCalendar(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	addEvent(t, d, "work", "cal-w", "Work", "e1", "Standup", testNow.Add(time.Hour), time.Hour)

	r := newTestRoot(t, d)
	send(t, r, "2")
	send(t, r, "r")
	send(t, r, "a")

	if _, ok := r.top().(*agenda); !ok {
		t.Errorf("top = %T, want the agenda", r.top())
	}
}

// Without an engine there is nothing to submit through, and the composer has
// to say so rather than sit on "sending…" for ever.
func TestSubmitWithoutAnEngineFailsTheComposerOpen(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	send(t, r, "ctrl+d")

	if _, ok := r.top().(*composeView); !ok {
		t.Fatalf("the composer closed on a failed send (top = %T)", r.top())
	}
	if c.sending {
		t.Error("the composer is still showing the send as in flight")
	}
	if c.err == nil {
		t.Error("no error on the composer")
	}
}

// ---------------------------------------------------------------------------
// Against a real engine and a fake provider, so a reply written here goes down
// exactly the path `emlcal mail reply` takes.

// addAnswerable indexes a message and gives the fake provider the same one,
// with the Message-ID a reply has to thread off.
func addAnswerable(t *testing.T, d Deps, mail *fake.Mail, remote, subject string) {
	t.Helper()
	when := testNow.Add(-time.Hour)
	raw := []byte("From: Anna de Vries <anna@example.com>\r\n" +
		"To: work@example.com\r\n" +
		"Message-ID: <orig@example.com>\r\n" +
		"Subject: " + subject + "\r\n\r\n" + subject + " body\r\n")
	mail.Add(fake.NewMsg(remote, raw).WithMailboxes("INBOX"))
	m := &model.Message{
		AccountID:       "work",
		RemoteID:        remote,
		ThreadID:        "t-" + remote,
		MessageIDHeader: "orig@example.com",
		Subject:         subject,
		From:            model.Address{Name: "Anna de Vries", Email: "anna@example.com"},
		To:              []model.Address{{Email: "work@example.com"}},
		Date:            when,
		Received:        when,
		TextBody:        subject + " body",
		MailboxRemotes:  []string{"INBOX"},
		IndexedAt:       testNow,
	}
	if _, err := d.Store.UpsertMessage(context.Background(), m, nil); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
}

func TestReplyReachesTheProviderAndMarksTheOriginalAnswered(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")

	r := newTestRoot(t, d)
	send(t, r, "r")
	for _, k := range []string{"o", "k"} {
		send(t, r, k)
	}
	send(t, r, "ctrl+d")

	sent := mail.Sent()
	if len(sent) != 1 {
		t.Fatalf("the provider was handed %d messages, want 1", len(sent))
	}
	parsed, err := mime.Parse(sent[0])
	if err != nil {
		t.Fatalf("what went out does not parse: %v", err)
	}
	if len(parsed.To) != 1 || parsed.To[0].Email != "anna@example.com" {
		t.Errorf("to = %v, want the sender", parsed.To)
	}
	if parsed.Subject != "Re: offerte Q4" {
		t.Errorf("subject = %q", parsed.Subject)
	}
	if parsed.InReplyTo != "orig@example.com" {
		t.Errorf("in-reply-to = %q, want the message it answers", parsed.InReplyTo)
	}
	if !strings.HasPrefix(parsed.TextBody, "ok") {
		t.Errorf("body does not start with what was typed:\n%s", parsed.TextBody)
	}
	if !strings.Contains(parsed.TextBody, "> offerte Q4 body") {
		t.Errorf("body does not quote the original:\n%s", parsed.TextBody)
	}

	flags, _, ok := mail.Lookup("m1")
	if !ok {
		t.Fatal("the original vanished from the provider")
	}
	if !flags.Answered {
		t.Error("the original was not marked answered")
	}

	if _, still := r.top().(*composeView); still {
		t.Errorf("the composer stayed open after the reply went out")
	}
	if r.status != "reply sent" {
		t.Errorf("status = %q, want %q", r.status, "reply sent")
	}
}

// A draft has not gone anywhere, so nothing has been answered.
func TestDraftFromTheComposerIsStoredAndAnswersNothing(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")

	r := newTestRoot(t, d)
	send(t, r, "r")
	send(t, r, "o")
	send(t, r, "ctrl+s")

	drafts := mail.Drafts()
	if len(drafts) != 1 {
		t.Fatalf("the provider was handed %d drafts, want 1", len(drafts))
	}
	if len(mail.Sent()) != 0 {
		t.Errorf("saving a draft sent %d messages", len(mail.Sent()))
	}
	if flags, _, _ := mail.Lookup("m1"); flags.Answered {
		t.Error("a stored draft marked the original answered")
	}
	if _, still := r.top().(*composeView); still {
		t.Error("the composer stayed open after the draft was stored")
	}
	if r.status != "draft saved" {
		t.Errorf("status = %q, want %q", r.status, "draft saved")
	}
}

// Offline, a reply that never left the machine is queued rather than lost, and
// the original stays unanswered until it really goes out.
func TestReplyOfflineIsQueuedAndAnswersNothingYet(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")

	r := newTestRoot(t, d)
	send(t, r, "r")
	send(t, r, "o")
	mail.FailNext(1)
	send(t, r, "ctrl+d")

	if len(mail.Sent()) != 0 {
		t.Errorf("the reply reached a provider that was meant to be unreachable")
	}
	if flags, _, _ := mail.Lookup("m1"); flags.Answered {
		t.Error("a queued reply marked the original answered")
	}
	if !strings.Contains(r.status, "queued") {
		t.Errorf("status = %q, want it to say the reply was queued", r.status)
	}
	if _, still := r.top().(*composeView); still {
		t.Error("the composer stayed open on a queued reply")
	}
}

// A screen that takes every key must not be able to hold the program.
func TestCtrlCQuitsOutOfTheComposer(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")
	send(t, r, "o")
	send(t, r, "ctrl+c")

	if !r.quitting {
		t.Error("ctrl+c was swallowed by the composer")
	}
}

// ---------------------------------------------------------------------------
// Finishing a stored draft

// addStoredDraft indexes a half-written reply the way a provider hands one
// back, and gives the fake provider the same message.
func addStoredDraft(t *testing.T, d Deps, mail *fake.Mail, remote string) {
	t.Helper()
	raw := []byte("From: work@example.com\r\nTo: anna@example.com\r\n" +
		"Subject: Re: offerte Q4\r\nIn-Reply-To: <orig@example.com>\r\n\r\nhalf a\r\n")
	mail.Add(fake.NewMsg(remote, raw).WithMailboxes("DRAFTS").
		WithFlags(model.Flags{Draft: true}))
	m := &model.Message{
		AccountID:      "work",
		RemoteID:       remote,
		ThreadID:       "t-m1",
		InReplyTo:      "orig@example.com",
		References:     []string{"orig@example.com"},
		Subject:        "Re: offerte Q4",
		From:           model.Address{Email: "work@example.com"},
		To:             []model.Address{{Email: "anna@example.com"}},
		Cc:             []model.Address{{Email: "carol@example.com"}},
		Date:           testNow.Add(-time.Minute),
		Received:       testNow.Add(-time.Minute),
		TextBody:       "half a\n\nOn Tue, 25 Aug 2026 at 11:00, Anna de Vries wrote:\n> Kun je dit bevestigen?",
		Flags:          model.Flags{Draft: true},
		MailboxRemotes: []string{"DRAFTS"},
		IndexedAt:      testNow,
	}
	if _, err := d.Store.UpsertMessage(context.Background(), m, nil); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
}

// A draft is not a message to read: enter on one reopens the editor.
func TestEnterOnADraftOpensItForEditing(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	send(t, r, "enter") // into the thread; the draft is the newest, on top
	send(t, r, "enter")

	c := composerOn(t, r)
	if c.draftRemote != "m2" {
		t.Errorf("editing %q, want the draft m2", c.draftRemote)
	}
	if c.to.Value() != "anna@example.com" || c.cc.Value() != "carol@example.com" {
		t.Errorf("to = %q cc = %q, want the draft's own recipients", c.to.Value(), c.cc.Value())
	}
	if c.subj.Value() != "Re: offerte Q4" {
		t.Errorf("subject = %q", c.subj.Value())
	}
	// The text goes back in whole: it is someone's unfinished writing, not
	// something to read, so the quote is not stripped off it.
	if !strings.HasPrefix(c.body.Value(), "half a") {
		t.Errorf("body = %q, want the draft as it was left", c.body.Value())
	}
	if !strings.Contains(c.body.Value(), "> Kun je dit bevestigen?") {
		t.Errorf("the quote was stripped out of the draft:\n%s", c.body.Value())
	}
	if !strings.Contains(r.render(), "draft · Re: offerte Q4") {
		t.Error("the header does not say this is a draft")
	}
}

func TestSendingAnEditedDraftGoesOutAndClearsTheOldOne(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	send(t, r, "enter")
	send(t, r, "enter")
	for _, k := range []string{"n", "d"} {
		send(t, r, k)
	}
	send(t, r, "ctrl+d")

	sent := mail.Sent()
	if len(sent) != 1 {
		t.Fatalf("the provider was handed %d messages, want 1", len(sent))
	}
	parsed, err := mime.Parse(sent[0])
	if err != nil {
		t.Fatalf("what went out does not parse: %v", err)
	}
	if !strings.HasPrefix(parsed.TextBody, "ndhalf a") {
		t.Errorf("body = %q, want the edit on top of what was there", parsed.TextBody)
	}
	if parsed.InReplyTo != "orig@example.com" {
		t.Errorf("in-reply-to = %q, want the draft's own threading kept", parsed.InReplyTo)
	}

	// A provider cannot update a draft in place, so the old one has to go or
	// it sits in the drafts mailbox for ever.
	_, boxes, ok := mail.Lookup("m2")
	if !ok {
		t.Fatal("the draft vanished from the provider")
	}
	if !contains(boxes, "TRASH") {
		t.Errorf("the draft that was sent is still filed in %v", boxes)
	}
	if r.status != "sent" {
		t.Errorf("status = %q, want %q", r.status, "sent")
	}
}

func TestSavingAnEditedDraftReplacesIt(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	send(t, r, "enter")
	send(t, r, "enter")
	send(t, r, "x")
	send(t, r, "ctrl+s")

	if len(mail.Drafts()) != 1 {
		t.Fatalf("the provider was handed %d drafts, want 1", len(mail.Drafts()))
	}
	if len(mail.Sent()) != 0 {
		t.Error("saving a draft sent something")
	}
	_, boxes, _ := mail.Lookup("m2")
	if !contains(boxes, "TRASH") {
		t.Errorf("the superseded draft is still filed in %v", boxes)
	}
	if r.status != "draft saved" {
		t.Errorf("status = %q, want %q", r.status, "draft saved")
	}
}

// An unfinished draft that fails to go out must still be there afterwards.
func TestAFailedSendLeavesTheDraftAlone(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	send(t, r, "enter")
	send(t, r, "enter")
	send(t, r, "x")
	mail.FailNext(1)
	send(t, r, "ctrl+d")

	_, boxes, ok := mail.Lookup("m2")
	if !ok {
		t.Fatal("the draft vanished from the provider")
	}
	if contains(boxes, "TRASH") {
		t.Errorf("a queued send trashed the draft it came from: %v", boxes)
	}
}

func TestEscapeOnAnUntouchedDraftClosesAtOnce(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	send(t, r, "enter")
	send(t, r, "enter")
	send(t, r, "esc")

	if _, ok := r.top().(*composeView); ok {
		t.Error("an untouched draft asked before closing")
	}
}

// Changing only a header is work too, and esc has to ask about it.
func TestEscapeAsksAfterAHeaderOnlyEdit(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	send(t, r, "tab") // onto To
	send(t, r, "x")

	send(t, r, "esc")
	if _, ok := r.top().(*composeView); !ok {
		t.Fatal("an edited recipient was thrown away without asking")
	}
	if c.pending != pendingDiscard {
		t.Error("the composer is not asking for a confirmation")
	}
}

// toDrafts cycles the mailbox filter round to drafts.
func toDrafts(t *testing.T, r *root) {
	t.Helper()
	for i := 0; i < len(mailboxCycle); i++ {
		if r.mail[0].(*mailList).showingDrafts() {
			return
		}
		send(t, r, "M")
	}
	t.Fatal("never reached the drafts mailbox")
}

// A row in the drafts mailbox is something unsent, not a conversation to read.
func TestEnterInTheDraftsViewOpensTheComposerAtOnce(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	toDrafts(t, r)
	if got := len(r.top().(*mailList).threads); got != 1 {
		t.Fatalf("the drafts view has %d rows, want 1", got)
	}
	send(t, r, "enter")

	c := composerOn(t, r)
	if c.draftRemote != "m2" {
		t.Errorf("editing %q, want the draft m2", c.draftRemote)
	}
}

// The draft is what enter opens even when the mail it answers is unread --
// which is where the thread would otherwise have put the cursor.
func TestEnterInTheDraftsViewIgnoresAnUnreadOriginal(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")
	m, err := d.Store.GetMessage(context.Background(), "work", "m1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	m.Flags.Unread = true
	if _, err := d.Store.UpsertMessage(context.Background(), m, nil); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}

	r := newTestRoot(t, d)
	toDrafts(t, r)
	send(t, r, "enter")

	c := composerOn(t, r)
	if c.draftRemote != "m2" {
		t.Errorf("editing %q, want the draft m2", c.draftRemote)
	}
}

// From the drafts view a send is enter then ctrl+d, and nothing in between.
func TestSendingStraightOutOfTheDraftsView(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	toDrafts(t, r)
	send(t, r, "enter")
	send(t, r, "ctrl+d")

	if len(mail.Sent()) != 1 {
		t.Fatalf("the provider was handed %d messages, want 1", len(mail.Sent()))
	}
	if r.status != "sent" {
		t.Errorf("status = %q, want %q", r.status, "sent")
	}
	if _, still := r.top().(*composeView); still {
		t.Error("the composer stayed open after the draft went out")
	}
}

// A row whose draft has gone -- sent from another client, say -- still opens
// as what it otherwise is: the conversation.
func TestADraftsRowWithNoDraftLeftOpensTheThread(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	toDrafts(t, r)
	// The daemon syncs the draft away underneath the cursor.
	m, err := d.Store.GetMessage(context.Background(), "work", "m2")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	m.Flags.Draft = false
	if _, err := d.Store.UpsertMessage(context.Background(), m, nil); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	send(t, r, "enter")

	if _, ok := r.top().(*threadView); !ok {
		t.Errorf("top = %T, want the thread as a fallback", r.top())
	}
	if strings.Contains(r.status, "compose:") {
		t.Errorf("status = %q, want no error for a row that simply moved on", r.status)
	}
}

// The drafts view says what enter does there.
func TestTheDraftsViewSaysWhatEnterDoes(t *testing.T) {
	d, mail := newTriageDeps(t)
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	toDrafts(t, r)
	if got := r.render(); !strings.Contains(got, "enter to finish one") {
		t.Errorf("the drafts view does not say what enter does:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// Throwing a draft away

// `d` on a drafts row is about the draft. Trashing the conversation it belongs
// to would take the mail it answers with it.
func TestTrashFromADraftsRowLeavesTheConversationAlone(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	toDrafts(t, r)
	send(t, r, "d")

	if _, boxes, _ := mail.Lookup("m2"); !contains(boxes, "TRASH") {
		t.Errorf("the draft was not trashed: %v", boxes)
	}
	if _, boxes, _ := mail.Lookup("m1"); contains(boxes, "TRASH") {
		t.Errorf("trashing the draft took the mail it answers with it: %v", boxes)
	}
}

func TestDeleteFromTheComposerAsksAndThenTrashesTheDraft(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	toDrafts(t, r)
	send(t, r, "enter")
	c := composerOn(t, r)

	send(t, r, "ctrl+x")
	if _, ok := r.top().(*composeView); !ok {
		t.Fatalf("one ctrl+x deleted the draft (top = %T)", r.top())
	}
	if c.pending != pendingDelete {
		t.Error("the composer is not asking for a confirmation")
	}
	if _, boxes, _ := mail.Lookup("m2"); contains(boxes, "TRASH") {
		t.Fatal("the draft was trashed on the first ctrl+x")
	}

	send(t, r, "ctrl+x")
	if _, boxes, _ := mail.Lookup("m2"); !contains(boxes, "TRASH") {
		t.Errorf("the draft was not trashed: %v", boxes)
	}
	if _, boxes, _ := mail.Lookup("m1"); contains(boxes, "TRASH") {
		t.Errorf("deleting the draft took the mail it answers with it: %v", boxes)
	}
	if _, still := r.top().(*composeView); still {
		t.Error("the composer stayed open after the draft was deleted")
	}
	if !strings.Contains(r.status, "draft deleted") {
		t.Errorf("status = %q", r.status)
	}
}

// Typing after the question is a change of mind.
func TestTypingCancelsThePendingDelete(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	toDrafts(t, r)
	send(t, r, "enter")
	c := composerOn(t, r)

	send(t, r, "ctrl+x")
	send(t, r, "x")
	send(t, r, "ctrl+x")

	if _, boxes, _ := mail.Lookup("m2"); contains(boxes, "TRASH") {
		t.Errorf("the draft was deleted without being asked about twice in a row: %v", boxes)
	}
	if c.pending != pendingDelete {
		t.Error("the second ctrl+x did not ask again")
	}
}

// A reply that was never saved has nothing stored to delete; esc is what
// abandons it, and the key says so rather than doing nothing.
func TestDeleteOnAnUnsavedReplySaysThereIsNothingStored(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	send(t, r, "ctrl+x")

	if c.pending == pendingDelete {
		t.Error("a reply with nothing stored asked to delete something")
	}
	if c.err == nil || !strings.Contains(c.err.Error(), "nothing stored") {
		t.Errorf("err = %v, want it to say there is nothing stored", c.err)
	}
	if _, ok := r.top().(*composeView); !ok {
		t.Errorf("ctrl+x closed the composer (top = %T)", r.top())
	}
}

// esc after ctrl+x asks about the work, not about the draft: cancelling a
// composer is never deleting.
func TestEscapeAfterDeleteAsksAboutTheWorkInstead(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	toDrafts(t, r)
	send(t, r, "enter")
	c := composerOn(t, r)
	send(t, r, "x") // something to lose
	send(t, r, "ctrl+x")
	send(t, r, "esc")

	if c.pending != pendingDiscard {
		t.Errorf("pending = %v, want the question to be about the unsaved work", c.pending)
	}
	if _, boxes, _ := mail.Lookup("m2"); contains(boxes, "TRASH") {
		t.Errorf("esc deleted the draft: %v", boxes)
	}
}

// The composer says the delete key is there only when there is one to press.
func TestTheComposerOffersDeleteOnlyForAStoredDraft(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "offerte Q4")
	addStoredDraft(t, d, mail, "m2")

	r := newTestRoot(t, d)
	send(t, r, "r") // continues the draft, so ctrl+x is on offer
	if got := r.render(); !strings.Contains(got, "ctrl+x delete") {
		t.Errorf("a stored draft does not offer delete:\n%s", got)
	}

	d2 := newTestDeps(t, "work")
	addConversation(t, d2, "work", "w1", "t1")
	r2 := newTestRoot(t, d2)
	send(t, r2, "r")
	if got := r2.render(); strings.Contains(got, "ctrl+x delete") {
		t.Errorf("a fresh reply offers a delete it cannot do:\n%s", got)
	}
}
