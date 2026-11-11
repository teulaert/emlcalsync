package sync

import (
	"context"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
)

func replyRaw(t *testing.T, inReplyTo, body string) []byte {
	t.Helper()
	raw, err := mime.Build(&mime.Draft{
		From:      model.Address{Email: "user@example.com"},
		To:        []model.Address{{Email: "anna@example.com"}},
		Subject:   "Re: budget",
		TextBody:  body,
		InReplyTo: inReplyTo,
		Date:      time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("mime.Build: %v", err)
	}
	return raw
}

// A reply drafted twice leaves the first version in Drafts: no provider
// updates a draft in place, and the composer only trashes the one it was
// finishing. Sending the reply makes every draft answering the same message
// obsolete, so the send trashes them -- and nothing else.
func TestSendTrashesTheDraftsItSuperseded(t *testing.T) {
	h := newHarness(t)
	h.mail.AddMailbox(model.Mailbox{RemoteID: "DRAFTS", Name: "Drafts", Role: model.RoleDrafts})
	draft := model.Flags{Draft: true}
	h.mail.Add(&fakeMsg{id: "v1", raw: replyRaw(t, "orig@example.com", "first go"),
		flags: draft, mailboxes: []string{"DRAFTS"}})
	h.mail.Add(&fakeMsg{id: "v2", raw: replyRaw(t, "orig@example.com", "second go"),
		flags: draft, mailboxes: []string{"DRAFTS", "ARCHIVE"}})
	h.mail.Add(&fakeMsg{id: "other", raw: replyRaw(t, "other@example.com", "unrelated"),
		flags: draft, mailboxes: []string{"DRAFTS"}})
	h.sync(SyncOptions{Mail: true})

	res, err := h.eng.Apply(context.Background(), "work",
		Op{Kind: OpSend, Raw: replyRaw(t, "orig@example.com", "final")})
	if err != nil || res.Queued {
		t.Fatalf("send: res=%+v err=%v", res, err)
	}

	for _, id := range []string{"v1", "v2"} {
		if got := h.providerMailboxes(id); !contains(got, "TRASH") {
			t.Errorf("%s on the provider is in %v, want it trashed", id, got)
		}
		if got := h.message(id).MailboxRemotes; !contains(got, "TRASH") {
			t.Errorf("%s in the index is in %v, want it trashed", id, got)
		}
	}
	if got := h.providerMailboxes("other"); contains(got, "TRASH") {
		t.Errorf("a draft answering another message was trashed: %v", got)
	}
}

// A new message answers nothing, so it supersedes no draft.
func TestSendOfANewMessageLeavesDraftsAlone(t *testing.T) {
	h := newHarness(t)
	h.mail.AddMailbox(model.Mailbox{RemoteID: "DRAFTS", Name: "Drafts", Role: model.RoleDrafts})
	h.mail.Add(&fakeMsg{id: "d1", raw: mailRaw(t, "hello", "draft"),
		flags: model.Flags{Draft: true}, mailboxes: []string{"DRAFTS"}})
	h.sync(SyncOptions{Mail: true})

	if _, err := h.eng.Apply(context.Background(), "work",
		Op{Kind: OpSend, Raw: mailRaw(t, "hello", "sent")}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := h.providerMailboxes("d1"); contains(got, "TRASH") {
		t.Errorf("a draft was trashed by an unrelated send: %v", got)
	}
}
