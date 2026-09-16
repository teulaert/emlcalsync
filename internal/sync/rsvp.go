package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/teulaert/emlcalsync/internal/itip"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
)

// RSVPResult is what answering an invitation by mail did.
type RSVPResult struct {
	// Apply is the send: queued when the provider could not be reached, with
	// the outbox holding the message until it can.
	Apply *ApplyResult
	// To is the organizer the answer went to, and Response what it said.
	To       model.Address
	Response model.Participation
	// Subject is the reply's subject line, which is the one thing the
	// organizer sees before their scheduler opens it.
	Subject string
}

// RespondByMail answers an invitation by mailing the organizer an iTIP REPLY,
// and records the answer against the message.
//
// This is the road for an invitation no calendar holds. Where the event has
// been filed -- the usual case, because a server that processes iMIP puts it
// on the calendar as the mail arrives -- the answer belongs on the event
// instead: OpEventRespond changes the PARTSTAT and the calendar server is
// what sends the REPLY. Doing both would tell the organizer twice, so the
// caller chooses one, and the calendar wins when there is a copy (see
// itip.Match, and the TUI's readerInvite).
//
// The bookkeeping afterwards is deliberately not conditional on the send
// having gone all the way out. A queued send is a send: it is committed to
// the outbox before Apply returns and the daemon retries it until the
// provider takes it. Waiting for that before recording the answer would
// leave the invitation asking to be answered for as long as the machine is
// offline -- and this is the one kind of message where nothing else remembers
// what was said, there being no event to carry it.
func (e *Engine) RespondByMail(ctx context.Context, account, messageRemote string,
	resp model.Participation) (*RSVPResult, error) {
	acct, ok := e.cfg.Account(account)
	if !ok {
		return nil, fmt.Errorf("sync: unknown account %q", account)
	}
	// The address, with no display name: an emlcal account has none, and
	// `mail reply` sends the same way. itip.Reply answers under the name the
	// organizer put on the attendee line, which is the better one anyway.
	self := model.Address{Email: acct.Email}
	if self.Email == "" {
		return nil, fmt.Errorf("sync: account %q has no address to answer from", account)
	}

	msg, err := e.st.GetMessage(ctx, account, messageRemote)
	if err != nil {
		return nil, err
	}
	raw, err := e.EnsureRaw(ctx, account, messageRemote)
	if err != nil {
		return nil, err
	}
	inv, err := itip.FromMessage(raw, self.Email)
	if err != nil {
		if errors.Is(err, itip.ErrNoInvite) {
			return nil, fmt.Errorf("message %s carries no invitation", msg.PublicID())
		}
		return nil, err
	}
	reply, err := inv.Reply(self, resp, time.Now())
	if err != nil {
		return nil, err
	}

	// The answer threads under the invitation. An organizer reading their own
	// mail sees it in the conversation the invitation started, which is where
	// they are looking; their scheduler ignores the headers and reads the
	// calendar part either way.
	draft := &mime.Draft{
		From:      self,
		To:        []model.Address{reply.To},
		Subject:   reply.Subject,
		TextBody:  reply.Text,
		InReplyTo: msg.MessageIDHeader,
		Calendar:  &mime.DraftCalendar{Method: itip.MethodReply, Content: reply.Calendar},
	}
	if msg.MessageIDHeader != "" {
		draft.References = append(append([]string(nil), msg.References...), msg.MessageIDHeader)
	}
	built, err := mime.Build(draft)
	if err != nil {
		return nil, fmt.Errorf("sync: build the RSVP: %w", err)
	}

	res, err := e.Apply(ctx, account, Op{
		Kind:       OpSend,
		Raw:        built,
		ThreadID:   msg.ThreadID,
		From:       self.Email,
		Recipients: []string{reply.To.Email},
	})
	if err != nil {
		return nil, err
	}

	// Neither of these may fail the RSVP: the organizer has been told, and
	// that is the part that cannot be taken back. What is left is the
	// archive's own memory of it.
	if err := e.st.SetITIPResponse(ctx, account, messageRemote, resp); err != nil {
		e.log.Warn("record the RSVP", "id", msg.PublicID(), "response", resp, "err", err)
	}
	var flagOp Op
	flagOp.Kind = OpFlags
	flagOp.IDs = []string{messageRemote}
	flagOp.Flags.Set.Answered = true
	if _, err := e.Apply(ctx, account, flagOp); err != nil {
		e.log.Warn("mark the invitation answered", "id", msg.PublicID(), "err", err)
	}

	return &RSVPResult{Apply: res, To: reply.To, Response: resp, Subject: reply.Subject}, nil
}
