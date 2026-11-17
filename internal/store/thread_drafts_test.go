package store

import (
	"context"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
)

// draftThreadFixture builds the case Lennert hit: a conversation with a reply
// that was really sent, and today's reply that is only a draft. Under Sent the
// thread must be dated and ordered by the sent reply, and say it holds a draft;
// it must not read as sent today.
//
// gmail selects the label shape: every mailbox is a label, so an older draft
// that was sent stays in Drafts and gains Trash. Otherwise the JMAP shape: one
// mailbox per message and no such leftovers.
func draftThreadFixture(t *testing.T, gmail bool) (*Store, time.Time, time.Time, time.Time) {
	t.Helper()
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "a")
	if err := s.ReplaceMailboxes(ctx, "a", []model.Mailbox{
		{RemoteID: "mb-inbox", Name: "Inbox", Role: model.RoleInbox, SortOrder: 1},
		{RemoteID: "mb-sent", Name: "Sent", Role: model.RoleSent, SortOrder: 2},
		{RemoteID: "mb-drafts", Name: "Drafts", Role: model.RoleDrafts, SortOrder: 3},
		{RemoteID: "mb-trash", Name: "Trash", Role: model.RoleTrash, SortOrder: 4},
	}); err != nil {
		t.Fatalf("ReplaceMailboxes: %v", err)
	}

	received := base
	sent := base.Add(24 * time.Hour)
	other := base.Add(36 * time.Hour) // another conversation, sent in between
	draft := base.Add(48 * time.Hour)

	put := func(remote, thread string, at time.Time, isDraft bool, boxes ...string) {
		putMessage(t, s, &model.Message{
			AccountID: "a", RemoteID: remote, ThreadID: thread, Received: at,
			Flags: model.Flags{Draft: isDraft}, MailboxRemotes: boxes,
		}, &mime.Parsed{
			Subject: "Re: toegang", From: addr("Me", "a@example.com"),
			To: []model.Address{addr("Anna", "anna@example.com")}, Date: at,
		})
	}
	put("in", "th-a", received, false, "mb-inbox")
	if gmail {
		// The draft of the reply that went out: Gmail keeps DRAFT, adds TRASH.
		put("sent-draft", "th-a", sent.Add(-time.Minute), true, "mb-drafts", "mb-trash")
	}
	put("reply", "th-a", sent, false, "mb-sent")
	put("draft", "th-a", draft, true, "mb-drafts")
	put("other", "th-b", other, false, "mb-sent")
	return s, received, sent, draft
}

func TestThreadWithADraftUnderSent(t *testing.T) {
	for _, shape := range []struct {
		name  string
		gmail bool
	}{{"gmail", true}, {"jmap", false}} {
		t.Run(shape.name, func(t *testing.T) {
			ctx := context.Background()
			s, received, sent, draft := draftThreadFixture(t, shape.gmail)
			byID := func(f MessageFilter) map[string]model.Thread {
				th, err := s.ListThreads(ctx, f)
				if err != nil {
					t.Fatalf("ListThreads: %v", err)
				}
				out := map[string]model.Thread{}
				for _, x := range th {
					out[x.ThreadID] = x
				}
				return out
			}

			// Sent: dated by the reply that went out, ordered after the
			// conversation sent later, and marked as holding one draft
			// (the trashed Gmail leftover is not one).
			th, err := s.ListThreads(ctx, MessageFilter{MailboxRole: "sent"})
			if err != nil {
				t.Fatalf("ListThreads sent: %v", err)
			}
			if len(th) != 2 || th[0].ThreadID != "th-b" || th[1].ThreadID != "th-a" {
				t.Fatalf("sent order = %v, want th-b (sent later) before th-a", threadIDs(th))
			}
			a := th[1]
			if !a.Last.Equal(sent) {
				t.Errorf("sent view dates th-a %v, want the sent reply %v (not the draft %v)", a.Last, sent, draft)
			}
			if a.Drafts != 1 {
				t.Errorf("th-a drafts = %d, want 1", a.Drafts)
			}
			if b := th[0]; b.Drafts != 0 {
				t.Errorf("th-b drafts = %d, want 0", b.Drafts)
			}

			// Paging in the sent view is keyed on the date it shows.
			page, err := s.ListThreads(ctx, MessageFilter{MailboxRole: "sent", Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			next, err := s.ListThreads(ctx, MessageFilter{MailboxRole: "sent", Limit: 1,
				AfterLast: page[0].Last, AfterThread: page[0].ThreadID, AfterAccount: page[0].AccountID})
			if err != nil {
				t.Fatal(err)
			}
			if got := threadIDs(append(page, next...)); !equalStrings(got, []string{"th-b", "th-a"}) {
				t.Errorf("sent pages = %v, want [th-b th-a]", got)
			}

			// Inbox: dated by what arrived there.
			if got := byID(MessageFilter{MailboxRole: "inbox"})["th-a"]; !got.Last.Equal(received) {
				t.Errorf("inbox view dates th-a %v, want %v", got.Last, received)
			}
			// Drafts: dated by the draft.
			if got := byID(MessageFilter{MailboxRole: "drafts"})["th-a"]; !got.Last.Equal(draft) || got.Drafts != 1 {
				t.Errorf("drafts view: last %v drafts %d, want %v and 1", got.Last, got.Drafts, draft)
			}
			// No mailbox: the thread's newest message, still marked.
			if got := byID(MessageFilter{})["th-a"]; !got.Last.Equal(draft) || got.Drafts != 1 {
				t.Errorf("all-mail view: last %v drafts %d, want %v and 1", got.Last, got.Drafts, draft)
			}
			// Opening the thread reports the draft too.
			tt, _, err := s.GetThread(ctx, "a", "th-a", false)
			if err != nil || tt.Drafts != 1 {
				t.Errorf("GetThread drafts = %v (err %v), want 1", tt, err)
			}
		})
	}
}

func threadIDs(th []model.Thread) []string {
	out := make([]string, 0, len(th))
	for _, x := range th {
		out = append(out, x.ThreadID)
	}
	return out
}
