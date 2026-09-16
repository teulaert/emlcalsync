package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
	"github.com/teulaert/emlcalsync/internal/sync"
)

const inviteUID = "040000008200E00074C5B7101A82E00800000000BB3DDF993738DD01000000000000000010000000D9B5581854DF3640B533A07A2B4B5089"

func openInvite(t *testing.T, onCalendar bool) (*root, Deps, *fake.Mail) {
	t.Helper()
	d, mail, cal := newTriageDepsWithCalendar(t)
	raw, err := os.ReadFile(filepath.Join("..", "mime", "testdata", "invite.eml"))
	if err != nil {
		t.Fatal(err)
	}
	mail.Add(fake.NewMsg("inv-1", raw).WithMailboxes("INBOX"))
	if onCalendar {
		start := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
		cal.Put("primary", model.Event{
			RemoteID: "ev-momentum", UID: inviteUID, Title: "Momentum FO",
			Start: start, End: start.Add(45 * time.Minute), Status: model.StatusConfirmed,
			Organizer:  model.Address{Name: "Martijn Organiser", Email: "martijn@example.org"},
			MyResponse: model.PartNeedsAction,
		})
	}
	if _, err := d.Engine.SyncAccount(context.Background(), "work", sync.SyncOptions{}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	r := newTestRoot(t, d)
	if got := len(r.mail[0].(*mailList).threads); got != 1 {
		t.Fatalf("list has %d threads, want the invitation", got)
	}
	send(t, r, "enter") // the thread
	send(t, r, "enter") // the reader
	rd, ok := r.top().(*reader)
	if !ok {
		t.Fatalf("top screen is %T, want the reader", r.top())
	}
	if rd.msg == nil {
		t.Fatal("reader has no message")
	}
	return r, d, mail
}

func TestReaderShowsTheInvitationAndAnswersIt(t *testing.T) {
	r, d, _ := openInvite(t, true)
	rd := r.top().(*reader)
	if rd.invite == nil || rd.invite.local == nil {
		t.Fatalf("reader carries no invite, or no calendar copy: %+v", rd.invite)
	}

	view := r.top().View(r.w, r.bodyHeight())
	for _, want := range []string{
		"Invitation: Momentum FO",
		"When:       Wed 2 Sep 08:00–08:45",
		"Where:      Microsoft Teams-vergadering",
		"Organizer:  Martijn Organiser <martijn@example.org>",
		"You:        not answered",
		"Answer:     y accept · n decline · t tentative",
		"Microsoft Teams meeting", // the text is still under the card
	} {
		if !strings.Contains(view, want) {
			t.Errorf("reader misses %q:\n%s", want, view)
		}
	}
	if f := rd.footer(80); !strings.Contains(f, "y accept") {
		t.Errorf("footer = %q, want the RSVP keys", f)
	}

	send(t, r, "y")

	ev, err := d.Store.GetEvent(context.Background(), "work", "primary", "ev-momentum")
	if err != nil {
		t.Fatal(err)
	}
	if ev.MyResponse != model.PartAccepted {
		t.Errorf("after y the calendar's copy says %q, want accepted", ev.MyResponse)
	}
	if !strings.Contains(r.status, "accepted") {
		t.Errorf("status = %q", r.status)
	}
	// The reader re-read the message: the card now says so, and stops
	// asking.
	view = r.top().View(r.w, r.bodyHeight())
	if !strings.Contains(view, "You:        yes") || strings.Contains(view, "Answer:") {
		t.Errorf("after accepting:\n%s", view)
	}
	// The keys stay, as they do on the event: an answer can be changed.
	if f := r.top().(*reader).footer(80); !strings.Contains(f, "y accept") {
		t.Errorf("footer dropped the RSVP keys after the answer: %q", f)
	}
	send(t, r, "n")
	ev, _ = d.Store.GetEvent(context.Background(), "work", "primary", "ev-momentum")
	if ev.MyResponse != model.PartDeclined {
		t.Errorf("after n the calendar's copy says %q, want declined", ev.MyResponse)
	}
}

func TestReaderOpensTheInvitedEvent(t *testing.T) {
	r, _, _ := openInvite(t, true)
	send(t, r, "enter")
	ev, ok := r.top().(*eventView)
	if !ok {
		t.Fatalf("enter on the invitation opened %T, want the event", r.top())
	}
	if ev.remote != "ev-momentum" || ev.calRemote != "primary" || ev.accountID != "work" {
		t.Errorf("event view is on %s/%s/%s", ev.accountID, ev.calRemote, ev.remote)
	}
}

// An invitation no calendar holds is still answerable: the reply is mailed to
// the organizer. This is the case that used to be a dead end -- the keys were
// not offered and the card said the event was "not on a synced calendar yet",
// which reads as "wait" for something the server was never going to file.
func TestReaderInvitationWithoutCalendarCopyAnswersByMail(t *testing.T) {
	r, d, mail := openInvite(t, false)
	rd := r.top().(*reader)
	if rd.invite == nil || rd.invite.local != nil {
		t.Fatalf("invite = %+v", rd.invite)
	}
	if !rd.invite.answerable() || !rd.invite.byMail() {
		t.Fatal("the invitation is not answerable by mail")
	}

	view := r.top().View(r.w, r.bodyHeight())
	for _, want := range []string{
		"Invitation: Momentum FO",
		// The card says where the answer goes: this one puts a message in
		// somebody's inbox, which is not what "accept" looks like it does.
		"Answer:     y accept · n decline · t tentative — replies to martijn@example.org",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("reader misses %q:\n%s", want, view)
		}
	}
	if f := rd.footer(80); !strings.Contains(f, "y accept") || !strings.Contains(f, "(by mail)") {
		t.Errorf("footer = %q, want the RSVP keys and the road they take", f)
	}

	send(t, r, "y")

	sent := mail.Sent()
	if len(sent) != 1 {
		t.Fatalf("%d messages went out, want the reply", len(sent))
	}
	if !strings.Contains(string(sent[0]), "method=REPLY") {
		t.Errorf("what went out is not an iTIP reply:\n%s", sent[0])
	}
	if !strings.Contains(r.status, "accepted") || !strings.Contains(r.status, "martijn@example.org") {
		t.Errorf("status = %q, want it to name where the answer went", r.status)
	}

	// The archive remembers what was said, the card stops asking, and the
	// keys stay so the answer can be changed.
	msg, err := d.Store.GetMessage(context.Background(), "work", "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if msg.ITIPResponse != model.PartAccepted {
		t.Errorf("the archive recorded %q, want accepted", msg.ITIPResponse)
	}
	view = r.top().View(r.w, r.bodyHeight())
	if !strings.Contains(view, "You:        yes") {
		t.Errorf("the card does not show the answer:\n%s", view)
	}
	if f := r.top().(*reader).footer(80); !strings.Contains(f, "y accept") {
		t.Errorf("footer dropped the RSVP keys after the answer: %q", f)
	}

	// Accepting also filed the meeting, so the status says so and the
	// invitation is now in the state one the server had filed would be in:
	// enter opens the event, under the invitation's own UID.
	if !strings.Contains(r.status, "calendar") {
		t.Errorf("status = %q, want it to say the meeting was filed too", r.status)
	}
	evs, err := d.Store.FindEventsByUID(context.Background(), []string{"work"}, inviteUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("the index holds %d events for the invitation's uid, want 1", len(evs))
	}
	send(t, r, "enter")
	ev, ok := r.top().(*eventView)
	if !ok {
		t.Fatalf("enter opened %T, want the event the accept filed", r.top())
	}
	if ev.remote != evs[0].RemoteID {
		t.Errorf("event view is on %q, want %q", ev.remote, evs[0].RemoteID)
	}
}

// With no calendar copy *and* no organizer there is nowhere for an answer to
// go. The keys go back to what they are everywhere else, rather than
// pretending to send something.
func TestReaderInvitationWithNowhereToAnswer(t *testing.T) {
	d, mail, _ := newTriageDepsWithCalendar(t)
	raw := "From: nobody@example.org\r\nTo: work@example.com\r\n" +
		"Subject: Orphan invitation\r\nMessage-ID: <orphan@example.org>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/calendar; charset=utf-8; method=REQUEST\r\n\r\n" +
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\n" +
		"UID:orphan-1\r\nDTSTART:20260918T090000Z\r\nDTEND:20260918T100000Z\r\n" +
		"SUMMARY:Orphan\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:work@example.com\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	mail.Add(fake.NewMsg("orphan-1", []byte(raw)).WithMailboxes("INBOX"))
	if _, err := d.Engine.SyncAccount(context.Background(), "work", sync.SyncOptions{}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	r := newTestRoot(t, d)
	send(t, r, "enter")
	send(t, r, "enter")
	rd, ok := r.top().(*reader)
	if !ok {
		t.Fatalf("top screen is %T, want the reader", r.top())
	}
	if rd.invite == nil {
		t.Fatal("the reader carries no invite")
	}
	if rd.invite.answerable() {
		t.Error("an invitation with no organizer and no calendar copy is answerable")
	}
	if !strings.Contains(r.top().View(r.w, r.bodyHeight()), "names no organizer") {
		t.Errorf("the card does not say why it cannot be answered:\n%s",
			r.top().View(r.w, r.bodyHeight()))
	}

	// y is copy-the-id here, as everywhere else; nothing is sent.
	send(t, r, "y")
	if !strings.Contains(r.status, "copied") {
		t.Errorf("status = %q, want the copy", r.status)
	}
	if n := len(mail.Sent()); n != 0 {
		t.Errorf("%d messages went out anyway", n)
	}
}

// The thread view is where mail is read first -- it opens on the text -- so
// the card is there too, and so are the keys.
func TestThreadViewShowsTheInvitationAndAnswersIt(t *testing.T) {
	r, d, _ := openInvite(t, true)
	send(t, r, "esc") // back out of the reader, onto the thread
	tv, ok := r.top().(*threadView)
	if !ok {
		t.Fatalf("top screen is %T, want the thread", r.top())
	}
	if !tv.expanded {
		send(t, r, "t")
	}
	view := r.top().View(r.w, r.bodyHeight())
	for _, want := range []string{
		"  Invitation: Momentum FO",
		"  When:       Wed 2 Sep 08:00–08:45",
		"  You:        not answered",
		"Answer:     y accept · n decline · t tentative",
		"  Microsoft Teams meeting",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("thread misses %q:\n%s", want, view)
		}
	}
	if f := tv.footer(80); !strings.Contains(f, "y accept") {
		t.Errorf("footer = %q", f)
	}

	send(t, r, "t") // tentative, not collapse
	if !tv.expanded {
		t.Error("t on an invitation collapsed the thread instead of answering")
	}
	ev, err := d.Store.GetEvent(context.Background(), "work", "primary", "ev-momentum")
	if err != nil {
		t.Fatal(err)
	}
	if ev.MyResponse != model.PartTentative {
		t.Errorf("after t the calendar's copy says %q, want tentative", ev.MyResponse)
	}
	view = r.top().View(r.w, r.bodyHeight())
	if !strings.Contains(view, "  You:        maybe") {
		t.Errorf("after answering:\n%s", view)
	}
}
