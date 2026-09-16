package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teulaert/emlcalsync/internal/itip"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// inviteFixture is the Exchange invitation the mime and itip tests share,
// readdressed to the account this harness runs as.
func inviteFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "mime", "testdata", "invite.eml"))
	if err != nil {
		t.Fatalf("read invite.eml: %v", err)
	}
	return []byte(strings.ReplaceAll(string(raw), "lennert@example.com", "user@example.com"))
}

// seedInvitation puts the invitation on the provider and syncs it in, so the
// RSVP runs against an indexed message with its raw bytes archived -- the
// state a message is in by the time anybody presses y on it.
func seedInvitation(t *testing.T, h *harness) model.Message {
	t.Helper()
	h.mail.Add(&fakeMsg{id: "inv-1", raw: inviteFixture(t)})
	h.sync(SyncOptions{Full: true})

	msg, err := h.st.GetMessage(context.Background(), "work", "inv-1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	return *msg
}

// sentInvite parses the one message the provider was handed.
func sentInvite(t *testing.T, h *harness) (*mime.Parsed, []byte) {
	t.Helper()
	sent := h.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("the provider was handed %d messages, want 1", len(sent))
	}
	p, err := mime.Parse(sent[0])
	if err != nil {
		t.Fatalf("the RSVP does not parse: %v\n%s", err, sent[0])
	}
	return p, sent[0]
}

func TestRespondByMailSendsTheReplyToTheOrganizer(t *testing.T) {
	h := newHarness(t)
	seedInvitation(t, h)

	res, err := h.eng.RespondByMail(context.Background(), "work", "inv-1", model.PartAccepted)
	if err != nil {
		t.Fatalf("RespondByMail: %v", err)
	}
	if res.To.Email != "martijn@example.org" {
		t.Errorf("answered to %+v, want the organizer", res.To)
	}
	if res.Subject != "Accepted: Momentum FO" {
		t.Errorf("subject = %q", res.Subject)
	}
	if res.Apply.Queued {
		t.Error("the send was queued although the provider was up")
	}

	p, raw := sentInvite(t, h)
	if p.Calendar == nil || p.Calendar.Method != "REPLY" {
		t.Fatalf("the message carries no REPLY: %+v", p.Calendar)
	}
	// It goes to the organizer and nobody else: the other attendees hear
	// about an RSVP from the organizer, if at all.
	if len(p.To) != 1 || p.To[0].Email != "martijn@example.org" {
		t.Errorf("To = %+v, want the organizer alone", p.To)
	}
	if len(p.Cc) != 0 {
		t.Errorf("Cc = %+v, want nobody", p.Cc)
	}

	ics, _, _, err := mime.PartContent(raw, p.Calendar.Path)
	if err != nil {
		t.Fatalf("PartContent: %v", err)
	}
	back, err := itip.Parse(ics, "user@example.com")
	if err != nil {
		t.Fatalf("the calendar part does not parse: %v", err)
	}
	if back.Method != itip.MethodReply {
		t.Errorf("method = %q", back.Method)
	}
	if len(back.Event.Attendees) != 1 || back.Event.Attendees[0].Response != model.PartAccepted {
		t.Errorf("attendees = %+v", back.Event.Attendees)
	}
	if back.Event.UID == "" {
		t.Error("the reply carries no UID, so the organizer cannot file it")
	}
}

// Nothing else remembers what was answered: there is no calendar copy, which
// is the whole reason this road exists. So the archive has to.
func TestRespondByMailRecordsTheAnswer(t *testing.T) {
	h := newHarness(t)
	seedInvitation(t, h)

	if _, err := h.eng.RespondByMail(context.Background(), "work", "inv-1", model.PartTentative); err != nil {
		t.Fatalf("RespondByMail: %v", err)
	}
	msg, err := h.st.GetMessage(context.Background(), "work", "inv-1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.ITIPResponse != model.PartTentative {
		t.Errorf("recorded %q, want tentative", msg.ITIPResponse)
	}
	if !msg.Flags.Answered {
		t.Error("the invitation was not marked answered")
	}

	// And it survives the delta that fetches the message again.
	h.mail.Update("inv-1", model.Flags{}, nil)
	h.sync(SyncOptions{})
	msg, err = h.st.GetMessage(context.Background(), "work", "inv-1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.ITIPResponse != model.PartTentative {
		t.Errorf("a sync wiped the answer: %q", msg.ITIPResponse)
	}
}

// A queued send is a send: the outbox holds it and the daemon retries until
// it goes. Waiting for the provider before recording the answer would leave
// the invitation asking to be answered for as long as the machine is offline.
func TestRespondByMailRecordsTheAnswerEvenWhenQueued(t *testing.T) {
	h := newHarness(t)
	seedInvitation(t, h)
	h.mail.FailNext(1) // the send cannot leave the machine

	res, err := h.eng.RespondByMail(context.Background(), "work", "inv-1", model.PartDeclined)
	if err != nil {
		t.Fatalf("RespondByMail: %v", err)
	}
	if !res.Apply.Queued {
		t.Fatal("the send was not queued although the provider was down")
	}
	msg, err := h.st.GetMessage(context.Background(), "work", "inv-1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.ITIPResponse != model.PartDeclined {
		t.Errorf("recorded %q, want declined", msg.ITIPResponse)
	}
}

func TestRespondByMailRefusesAMessageWithNoInvitation(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "plain-1", raw: []byte(
		"From: someone@example.org\r\nSubject: Just a note\r\n\r\nNo calendar here.\r\n")})
	h.sync(SyncOptions{Full: true})

	_, err := h.eng.RespondByMail(context.Background(), "work", "plain-1", model.PartAccepted)
	if err == nil {
		t.Fatal("answered a message that carries no invitation")
	}
	if !strings.Contains(err.Error(), "no invitation") {
		t.Errorf("err = %v, want it to say there is no invitation", err)
	}
	if n := len(h.sentMessages()); n != 0 {
		t.Errorf("%d messages went out anyway", n)
	}
}

func TestRespondByMailRefusesANonAnswer(t *testing.T) {
	h := newHarness(t)
	seedInvitation(t, h)

	if _, err := h.eng.RespondByMail(context.Background(), "work", "inv-1", model.PartNeedsAction); err == nil {
		t.Error("needs-action was accepted as an answer")
	}
	if n := len(h.sentMessages()); n != 0 {
		t.Errorf("%d messages went out anyway", n)
	}
}

// The answer threads under the invitation, so an organizer reading their own
// mail finds it in the conversation they started.
func TestRespondByMailThreadsUnderTheInvitation(t *testing.T) {
	h := newHarness(t)
	msg := seedInvitation(t, h)
	if msg.MessageIDHeader == "" {
		t.Skip("the fixture carries no Message-ID")
	}

	if _, err := h.eng.RespondByMail(context.Background(), "work", "inv-1", model.PartAccepted); err != nil {
		t.Fatalf("RespondByMail: %v", err)
	}
	_, raw := sentInvite(t, h)
	if !strings.Contains(string(raw), "In-Reply-To: <"+msg.MessageIDHeader+">") {
		t.Errorf("the reply does not answer the invitation:\n%s", raw)
	}
}

// Accepting a meeting that then does not appear on the agenda is not
// accepting it in any sense the person meant. The copy is filed with the
// invitation's UID, which is what ties it back to the mail it came from.
func TestRespondByMailFilesTheEventOnTheCalendar(t *testing.T) {
	h := newHarness(t)
	seedInvitation(t, h)

	res, err := h.eng.RespondByMail(context.Background(), "work", "inv-1", model.PartAccepted)
	if err != nil {
		t.Fatalf("RespondByMail: %v", err)
	}
	if res.EventErr != nil {
		t.Fatalf("no event was filed: %v", res.EventErr)
	}
	if res.Event == nil {
		t.Fatal("no event came back")
	}

	// Filed silently. A plain create is what would have made the calendar
	// server send a second REPLY, on top of the one already mailed.
	imported := h.cal.importedEvents()
	if len(imported) != 1 {
		t.Fatalf("%d events imported, want 1 (a create would reply twice)", len(imported))
	}
	got := imported[0]
	if got.UID != res.Event.UID || got.UID == "" {
		t.Errorf("filed under uid %q, want the invitation's", got.UID)
	}
	if got.Title != "Momentum FO" {
		t.Errorf("title = %q", got.Title)
	}
	if got.Organizer.Email != "martijn@example.org" {
		t.Errorf("organizer = %+v, want the one who invited", got.Organizer)
	}
	if got.MyResponse != model.PartAccepted {
		t.Errorf("my response on the filed copy = %q", got.MyResponse)
	}
	var self *model.Attendee
	for i := range got.Attendees {
		if got.Attendees[i].Self {
			self = &got.Attendees[i]
		}
	}
	if self == nil || self.Response != model.PartAccepted {
		t.Errorf("attendees = %+v, want the account marked self and accepted", got.Attendees)
	}

	// And the index knows it, which is what makes the card name the event and
	// a later change of mind go the calendar road.
	evs, err := h.st.FindEventsByUID(context.Background(), []string{"work"}, got.UID)
	if err != nil {
		t.Fatalf("FindEventsByUID: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("the index holds %d events for the invitation's uid, want 1", len(evs))
	}
}

// Declining is answered, not attended. Filing a meeting somebody has just
// said no to would put it on the agenda as though they were going.
func TestRespondByMailDoesNotFileADeclinedMeeting(t *testing.T) {
	h := newHarness(t)
	seedInvitation(t, h)

	res, err := h.eng.RespondByMail(context.Background(), "work", "inv-1", model.PartDeclined)
	if err != nil {
		t.Fatalf("RespondByMail: %v", err)
	}
	if res.Event != nil || res.EventErr != nil {
		t.Errorf("a declined meeting was filed: %+v / %v", res.Event, res.EventErr)
	}
	if n := len(h.cal.importedEvents()); n != 0 {
		t.Errorf("%d events filed for a decline", n)
	}
	// The reply still went.
	if len(h.sentMessages()) != 1 {
		t.Error("the decline did not go out")
	}
}

// A tentative answer is an answer, and the meeting still wants to be on the
// agenda — that is what "maybe" means about a slot.
func TestRespondByMailFilesATentativeMeeting(t *testing.T) {
	h := newHarness(t)
	seedInvitation(t, h)

	res, err := h.eng.RespondByMail(context.Background(), "work", "inv-1", model.PartTentative)
	if err != nil {
		t.Fatalf("RespondByMail: %v", err)
	}
	if res.Event == nil || res.Event.MyResponse != model.PartTentative {
		t.Errorf("event = %+v", res.Event)
	}
}

// noImportCalendar is a backend with no way to file an event silently. It
// embeds the interface rather than the fake, so ImportEvent is not in its
// method set and the type assertion in executeEvent fails the way it would
// against a real backend that does not implement provider.EventImporter.
type noImportCalendar struct{ provider.CalendarProvider }

// Such a backend must refuse rather than fall back to CreateEvent: a create
// is what makes the calendar server mail the organizer a REPLY, and one has
// already gone to them. Two answers to one invitation is the bug this whole
// road exists to avoid.
func TestRespondByMailRefusesToFileWhereItWouldReplyTwice(t *testing.T) {
	h := newHarness(t)
	h.fact.cal = noImportCalendar{h.cal}
	seedInvitation(t, h)

	res, err := h.eng.RespondByMail(context.Background(), "work", "inv-1", model.PartAccepted)
	if err != nil {
		t.Fatalf("RespondByMail: %v", err)
	}
	if res.EventErr == nil {
		t.Fatal("a backend that cannot file silently was not refused")
	}
	if res.Event != nil {
		t.Errorf("event = %+v, want none", res.Event)
	}
	if n := len(h.cal.importedEvents()); n != 0 {
		t.Errorf("%d events filed anyway", n)
	}

	// The RSVP is the part that cannot be taken back, and it stands: the
	// organizer was told, and the archive recorded what was said.
	if len(h.sentMessages()) != 1 {
		t.Fatal("the reply did not go out")
	}
	msg, err := h.st.GetMessage(context.Background(), "work", "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if msg.ITIPResponse != model.PartAccepted {
		t.Errorf("recorded %q", msg.ITIPResponse)
	}
}
