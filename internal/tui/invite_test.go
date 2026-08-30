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

func openInvite(t *testing.T, onCalendar bool) (*root, Deps) {
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
	return r, d
}

func TestReaderShowsTheInvitationAndAnswersIt(t *testing.T) {
	r, d := openInvite(t, true)
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
	r, _ := openInvite(t, true)
	send(t, r, "enter")
	ev, ok := r.top().(*eventView)
	if !ok {
		t.Fatalf("enter on the invitation opened %T, want the event", r.top())
	}
	if ev.remote != "ev-momentum" || ev.calRemote != "primary" || ev.accountID != "work" {
		t.Errorf("event view is on %s/%s/%s", ev.accountID, ev.calRemote, ev.remote)
	}
}

func TestReaderInvitationWithoutCalendarCopy(t *testing.T) {
	r, _ := openInvite(t, false)
	rd := r.top().(*reader)
	if rd.invite == nil || rd.invite.local != nil {
		t.Fatalf("invite = %+v", rd.invite)
	}
	view := r.top().View(r.w, r.bodyHeight())
	if !strings.Contains(view, "Invitation: Momentum FO") || !strings.Contains(view, "not on a synced calendar yet") {
		t.Errorf("reader:\n%s", view)
	}
	// y is copy-the-id here, as everywhere else; nothing is sent.
	send(t, r, "y")
	if !strings.Contains(r.status, "copied") {
		t.Errorf("status = %q, want the copy", r.status)
	}
	// And enter has nothing to open.
	send(t, r, "enter")
	if _, ok := r.top().(*reader); !ok {
		t.Errorf("enter left the reader for %T", r.top())
	}
}

// The thread view is where mail is read first -- it opens on the text -- so
// the card is there too, and so are the keys.
func TestThreadViewShowsTheInvitationAndAnswersIt(t *testing.T) {
	r, d := openInvite(t, true)
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
