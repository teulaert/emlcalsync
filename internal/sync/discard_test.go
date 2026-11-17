package sync

import (
	"context"
	"testing"

	"github.com/teulaert/emlcalsync/internal/model"
)

// discardingMail is the fake with provider.DraftDiscarder, the way Gmail and
// JMAP have it: drafts are deleted, anything else is left alone.
type discardingMail struct {
	*fakeMail
	discarded []string
}

func (d *discardingMail) DiscardDrafts(ctx context.Context, ids []string) error {
	for _, id := range ids {
		d.fakeMail.mu.Lock()
		m, ok := d.fakeMail.msgs[id]
		draft := ok && m.flags.Draft
		d.fakeMail.mu.Unlock()
		if draft {
			d.discarded = append(d.discarded, id)
			d.fakeMail.Remove(id)
		}
	}
	return nil
}

// A sent draft is deleted on the server, not trashed: a trashed Gmail draft
// keeps DRAFT beside TRASH, a trashed JMAP draft keeps $draft, and one such
// leftover piled up per send. The next delta drops it from the index.
func TestDiscardDraftsDeletesWhereTheProviderCan(t *testing.T) {
	h := newHarness(t)
	dm := &discardingMail{fakeMail: h.mail}
	h.fact.mail = dm
	h.mail.Add(&fakeMsg{id: "d1", raw: mailRaw(t, "draft", "half"), flags: model.Flags{Draft: true}, mailboxes: []string{"DRAFT"}})
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "mail", "real"), mailboxes: []string{"INBOX"}})
	h.sync(SyncOptions{Mail: true})

	if _, err := h.eng.Apply(context.Background(), "work", Op{Kind: OpDiscardDrafts, IDs: []string{"d1", "m1"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(dm.discarded) != 1 || dm.discarded[0] != "d1" {
		t.Fatalf("discarded = %v, want only the draft d1", dm.discarded)
	}
	if _, _, ok := h.mail.lookup("d1"); ok {
		t.Error("the draft is still on the server")
	}
	if _, boxes, ok := h.mail.lookup("m1"); !ok || contains(boxes, "TRASH") {
		t.Errorf("a non-draft was touched on the server: ok=%v boxes=%v", ok, boxes)
	}
	h.sync(SyncOptions{Mail: true})
	if got, err := h.st.GetMessage(context.Background(), "work", "d1"); err == nil && got.DeletedAt == nil {
		t.Error("the index still holds the discarded draft as live")
	}
}

// lookup reports a message's server-side mailboxes.
func (f *fakeMail) lookup(id string) (model.Flags, []string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.msgs[id]
	if !ok {
		return model.Flags{}, nil, false
	}
	return m.flags, append([]string(nil), m.mailboxes...), true
}

// A backend without DraftDiscarder (IMAP) still gets the draft out of the
// way: it is trashed, as before.
func TestDiscardDraftsTrashesWhereTheProviderCannot(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "d1", raw: mailRaw(t, "draft", "half"), flags: model.Flags{Draft: true}, mailboxes: []string{"DRAFT"}})
	h.sync(SyncOptions{Mail: true})

	if _, err := h.eng.Apply(context.Background(), "work", Op{Kind: OpDiscardDrafts, IDs: []string{"d1"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, boxes, ok := h.mail.lookup("d1"); !ok || !contains(boxes, "TRASH") {
		t.Errorf("server mailboxes = %v (present %v), want the draft trashed", boxes, ok)
	}
}
