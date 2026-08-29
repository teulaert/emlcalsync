package compose

import (
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
)

var testNow = time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC)

func original() *model.Message {
	return &model.Message{
		AccountID:       "work",
		RemoteID:        "m1",
		ThreadID:        "t1",
		MessageIDHeader: "orig@example.com",
		References:      []string{"root@example.com"},
		Subject:         "offerte Q4",
		From:            model.Address{Name: "Anna de Vries", Email: "anna@example.com"},
		To:              []model.Address{{Email: "me@example.com"}, {Email: "bob@example.com"}},
		Cc:              []model.Address{{Email: "carol@example.com"}},
		Date:            testNow,
		TextBody:        "Kun je dit bevestigen?\n\nOp eerder schreef iemand:\n> oud",
	}
}

func TestReplyGoesBackToTheSender(t *testing.T) {
	d := &mime.Draft{From: model.Address{Email: "me@example.com"}}
	Reply(d, original(), false, []string{"me@example.com"})

	if d.Subject != "Re: offerte Q4" {
		t.Errorf("subject = %q, want %q", d.Subject, "Re: offerte Q4")
	}
	if len(d.To) != 1 || d.To[0].Email != "anna@example.com" {
		t.Errorf("to = %v, want just the sender", d.To)
	}
	if len(d.Cc) != 0 {
		t.Errorf("cc = %v, want none on a plain reply", d.Cc)
	}
	if d.InReplyTo != "orig@example.com" {
		t.Errorf("in-reply-to = %q", d.InReplyTo)
	}
	want := []string{"root@example.com", "orig@example.com"}
	if len(d.References) != 2 || d.References[0] != want[0] || d.References[1] != want[1] {
		t.Errorf("references = %v, want %v", d.References, want)
	}
}

func TestReplyPrefersReplyTo(t *testing.T) {
	orig := original()
	orig.ReplyTo = []model.Address{{Email: "list@example.com"}}
	d := &mime.Draft{}
	Reply(d, orig, false, nil)
	if len(d.To) != 1 || d.To[0].Email != "list@example.com" {
		t.Errorf("to = %v, want the Reply-To the sender asked for", d.To)
	}
}

// The recipient the caller put there is a choice, not a default to improve on.
func TestReplyKeepsRecipientsAlreadySet(t *testing.T) {
	d := &mime.Draft{To: []model.Address{{Email: "someone@else.com"}}, Subject: "Not a reply"}
	Reply(d, original(), false, nil)
	if len(d.To) != 1 || d.To[0].Email != "someone@else.com" {
		t.Errorf("to = %v, want it left alone", d.To)
	}
	if d.Subject != "Not a reply" {
		t.Errorf("subject = %q, want it left alone", d.Subject)
	}
}

func TestReplyAllKeepsTheOthersAndDropsYourOwnAddress(t *testing.T) {
	d := &mime.Draft{From: model.Address{Email: "me@example.com"}}
	Reply(d, original(), true, []string{"me@example.com"})

	var to []string
	for _, a := range d.To {
		to = append(to, a.Email)
	}
	if strings.Join(to, ",") != "anna@example.com,bob@example.com" {
		t.Errorf("to = %v, want the sender and the other recipient, not me", to)
	}
	if len(d.Cc) != 1 || d.Cc[0].Email != "carol@example.com" {
		t.Errorf("cc = %v, want carol", d.Cc)
	}
}

func TestReplySubjectDoesNotStackRePrefixes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"offerte Q4", "Re: offerte Q4"},
		{"Re: offerte Q4", "Re: offerte Q4"},
		{"RE: offerte Q4", "RE: offerte Q4"},
		{"  spaced  ", "Re: spaced"},
	} {
		if got := ReplySubject(tc.in); got != tc.want {
			t.Errorf("ReplySubject(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Quoting the whole thread again on every round is how a reply chain doubles
// in size; only what the sender actually typed is quoted back.
func TestQuoteKeepsOnlyWhatTheSenderWrote(t *testing.T) {
	got := Quote(original(), time.UTC)
	if !strings.HasPrefix(got, "On Tue, 25 Aug 2026 at 11:00, Anna de Vries wrote:\n") {
		t.Errorf("attribution line = %q", strings.SplitN(got, "\n", 2)[0])
	}
	if !strings.Contains(got, "> Kun je dit bevestigen?") {
		t.Errorf("quote does not carry the text:\n%s", got)
	}
	if strings.Contains(got, "oud") {
		t.Errorf("quote carries the round before it:\n%s", got)
	}
}

func TestQuoteFallsBackToReceivedWhenThereIsNoDate(t *testing.T) {
	orig := original()
	orig.Date = time.Time{}
	orig.Received = testNow
	if !strings.HasPrefix(Quote(orig, time.UTC), "On Tue, 25 Aug 2026 at 11:00,") {
		t.Errorf("no date fell back to nothing:\n%s", Quote(orig, time.UTC))
	}
}

// A forward hands the whole thing to somebody who has seen none of it, which
// is the opposite of what a quote does.
func TestForwardedCarriesTheMessageWhole(t *testing.T) {
	got := Forwarded(original(), time.UTC)
	for _, want := range []string{
		"---------- Forwarded message ----------",
		"From: Anna de Vries <anna@example.com>",
		"Date: Tue, 25 Aug 2026 at 11:00",
		"Subject: offerte Q4",
		"To: me@example.com, bob@example.com",
		"Cc: carol@example.com",
		"Kun je dit bevestigen?",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the forwarded message lacks %q:\n%s", want, got)
		}
	}
	// The round before this one goes too: the person it is being sent to was
	// not in any of them.
	if !strings.Contains(got, "> oud") {
		t.Errorf("forwarding stripped the earlier round:\n%s", got)
	}
	// And none of it is marked as quoted -- the header block says where it
	// came from, so re-marking it would only say it twice.
	if strings.Contains(got, "> Kun je dit bevestigen?") {
		t.Errorf("the forwarded body was quoted:\n%s", got)
	}
}

func TestForwardedLeavesOutEmptyHeaders(t *testing.T) {
	orig := original()
	orig.Cc = nil
	if strings.Contains(Forwarded(orig, time.UTC), "Cc:") {
		t.Errorf("a message with no Cc got one:\n%s", Forwarded(orig, time.UTC))
	}
}

func TestForwardSubjectDoesNotStackPrefixes(t *testing.T) {
	for in, want := range map[string]string{
		"offerte Q4":      "Fwd: offerte Q4",
		"Fwd: offerte Q4": "Fwd: offerte Q4",
		"FWD: offerte Q4": "FWD: offerte Q4",
		"Fw: offerte Q4":  "Fw: offerte Q4",
		"Re: offerte Q4":  "Fwd: Re: offerte Q4",
	} {
		if got := ForwardSubject(in); got != want {
			t.Errorf("ForwardSubject(%q) = %q, want %q", in, got, want)
		}
	}
}

// Bcc is deliberately not in the message bytes, so the envelope is the only
// place a submission learns to deliver there.
func TestEnvelopeCarriesBccAndDeduplicates(t *testing.T) {
	d := &mime.Draft{
		To:  []model.Address{{Email: "a@x.com"}, {Email: "b@x.com"}},
		Cc:  []model.Address{{Email: "A@X.com"}},
		Bcc: []model.Address{{Email: "hidden@x.com"}},
	}
	got := Envelope(d)
	want := []string{"a@x.com", "b@x.com", "hidden@x.com"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("envelope = %v, want %v", got, want)
	}
}

func TestSplitAddressesRespectsQuotedDisplayNames(t *testing.T) {
	got := SplitAddresses(`"Doe, Jane" <j@x.com>, b@y.com`)
	if len(got) != 2 || got[0] != `"Doe, Jane" <j@x.com>` || got[1] != "b@y.com" {
		t.Errorf("split = %q", got)
	}
}

func TestParseAddressListRejectsRubbish(t *testing.T) {
	if _, err := ParseAddressList([]string{"not an address"}); err == nil {
		t.Error("parsed an address that is not one")
	}
	got, err := ParseAddressList([]string{"Anna <anna@x.com>, bob@y.com"})
	if err != nil {
		t.Fatalf("ParseAddressList: %v", err)
	}
	if len(got) != 2 || got[0].Name != "Anna" || got[1].Email != "bob@y.com" {
		t.Errorf("parsed %+v", got)
	}
}

// JoinAddresses feeds an editable To field, so it has to round-trip.
func TestJoinAddressesRoundTrips(t *testing.T) {
	in := []model.Address{{Name: "Doe, Jane", Email: "j@x.com"}, {Email: "b@y.com"}}
	back, err := ParseAddressList([]string{JoinAddresses(in)})
	if err != nil {
		t.Fatalf("re-parsing %q: %v", JoinAddresses(in), err)
	}
	if len(back) != 2 || back[0].Name != "Doe, Jane" || back[1].Email != "b@y.com" {
		t.Errorf("round trip gave %+v from %q", back, JoinAddresses(in))
	}
}
