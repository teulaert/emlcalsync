package imap

import (
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/testutil/imapfake"
)

// ids collects the remote ids out of a slice of envelopes.
func ids(envs []provider.Envelope) []string {
	out := make([]string, 0, len(envs))
	for _, e := range envs {
		out = append(out, e.RemoteID)
	}
	return out
}

func TestChangesReportsNewMail(t *testing.T) {
	m, srv := dial(t)
	ctx := testCtx(t)

	state, err := m.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	srv.Mail("INBOX", "brand new", "hello")

	ch, err := m.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(ch.Added) != 1 {
		t.Fatalf("added %v, want 1", ids(ch.Added))
	}
	if len(ch.Removed) != 0 {
		t.Errorf("removed %v, want none", ch.Removed)
	}
	if ch.NewState == "" {
		t.Error("no new state")
	}
}

// The common case must be cheap and, more importantly, must not invent changes.
func TestChangesOnAQuietAccountReportsNothing(t *testing.T) {
	m, srv := dial(t)
	ctx := testCtx(t)
	srv.Mail("INBOX", "settled", "hello")

	state, err := m.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	ch, err := m.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(ch.Added)+len(ch.Removed)+len(ch.Updated)+len(ch.Renamed) != 0 {
		t.Errorf("a quiet account produced changes: +%d -%d ~%d ->%d",
			len(ch.Added), len(ch.Removed), len(ch.Updated), len(ch.Renamed))
	}
}

// An expunge has to be noticed even though nothing on the wire announces it --
// this is what the uid set in the state is for.
func TestChangesDetectsExpunge(t *testing.T) {
	m, srv := dial(t)
	ctx := testCtx(t)
	srv.Mail("INBOX", "doomed", "hello")
	srv.Mail("INBOX", "survivor", "hello")

	state, err := m.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}

	// Delete the first message the way another client would.
	var victim string
	for _, e := range enumerateAll(t, m, 10) {
		if e.Mailboxes[0] == "INBOX" {
			victim = e.RemoteID
			break
		}
	}
	if _, err := m.TrashRemap(ctx, []string{victim}); err != nil {
		t.Fatalf("TrashRemap: %v", err)
	}

	ch, err := m.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	found := false
	for _, r := range ch.Removed {
		if r == victim {
			found = true
		}
	}
	if !found {
		t.Errorf("removed %v, want it to include %q", ch.Removed, victim)
	}
}

// A flag change with no arrival and no expunge is invisible on a server with no
// CONDSTORE, unless the client goes looking. The fake has no CONDSTORE, so this
// exercises the fallback -- the path most self-hosted servers take.
func TestChangesNoticesFlagChangesWithoutCondstore(t *testing.T) {
	m, srv := dial(t)
	ctx := testCtx(t)
	srv.Mail("INBOX", "will be read", "hello")

	state, err := m.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	id := enumerateAll(t, m, 10)[0].RemoteID

	if err := m.SetFlags(ctx, []string{id}, model.Flags{}, model.Flags{Unread: true}); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}

	ch, err := m.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	var got *provider.Envelope
	for i := range ch.Updated {
		if ch.Updated[i].RemoteID == id {
			got = &ch.Updated[i]
		}
	}
	if got == nil {
		t.Fatalf("updated %v, want it to include %q", ids(ch.Updated), id)
	}
	if got.Flags.Unread {
		t.Error("the update still reports the message as unread")
	}
}

// A folder recreated under us renumbers everything. That must come back as
// exact Removed plus Added, not as an expired state: expiring would force a
// whole-account reconcile over one folder.
func TestChangesHandlesUIDValidityResetInBand(t *testing.T) {
	m, srv := dial(t)
	ctx := testCtx(t)
	srv.CreateMailbox("Volatile")
	srv.Mail("Volatile", "before", "hello")

	state, err := m.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	var old string
	for _, e := range enumerateAll(t, m, 50) {
		if e.Mailboxes[0] == "Volatile" {
			old = e.RemoteID
		}
	}

	// Drop and recreate: same name, new UIDVALIDITY, different content.
	srv.DeleteMailbox("Volatile")
	srv.CreateMailbox("Volatile")
	srv.Mail("Volatile", "after", "hello again")

	ch, err := m.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v (a uidvalidity reset must not expire the state)", err)
	}
	if !containsStr(ch.Removed, old) {
		t.Errorf("removed %v, want the pre-reset id %q", ch.Removed, old)
	}
	if len(ch.Added) == 0 {
		t.Error("nothing added; the folder's new contents should be new to us")
	}
}

// A deleted folder is likewise expressible exactly.
func TestChangesHandlesDeletedFolderInBand(t *testing.T) {
	m, srv := dial(t)
	ctx := testCtx(t)
	srv.CreateMailbox("Temporary")
	srv.Mail("Temporary", "doomed folder", "hello")

	state, err := m.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	srv.DeleteMailbox("Temporary")

	ch, err := m.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(ch.Removed) != 1 {
		t.Fatalf("removed %v, want the one message the folder held", ch.Removed)
	}
	if !ch.MailboxesChanged {
		t.Error("MailboxesChanged should be set when a folder disappears")
	}
}

// A rename must move the messages, not delete and re-download a whole folder.
func TestChangesReportsAFolderRenameAsRenames(t *testing.T) {
	m, srv := dial(t)
	ctx := testCtx(t)
	srv.CreateMailbox("Projects")
	srv.Mail("Projects", "keep me", "hello")

	state, err := m.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	srv.RenameMailbox("Projects", "Work")

	ch, err := m.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(ch.Renamed) != 1 {
		t.Fatalf("renamed %+v, want one; removed=%v added=%v",
			ch.Renamed, ch.Removed, ids(ch.Added))
	}
	if len(ch.Removed) != 0 {
		t.Errorf("removed %v; a rename must not delete anything", ch.Removed)
	}
	if len(ch.Added) != 0 {
		t.Errorf("added %v; a rename must not re-download anything", ids(ch.Added))
	}
	oldRef, _ := parseRef(ch.Renamed[0].Old)
	newRef, _ := parseRef(ch.Renamed[0].New)
	if oldRef.Mailbox != "Projects" || newRef.Mailbox != "Work" {
		t.Errorf("rename %q -> %q does not describe the folder rename",
			oldRef.Mailbox, newRef.Mailbox)
	}
}

// An unreadable state is the one thing that genuinely forces a reconcile.
func TestChangesExpiresOnlyForAnUnreadableState(t *testing.T) {
	m, _ := dial(t)
	for _, bad := range []string{"", "not json", `{"v":999,"f":{}}`} {
		if _, err := m.Changes(testCtx(t), bad); err != provider.ErrStateExpired {
			t.Errorf("Changes(%q) = %v, want ErrStateExpired", bad, err)
		}
	}
}

// A new folder's whole contents arrive as Added.
func TestChangesPicksUpANewFolder(t *testing.T) {
	m, srv := dial(t)
	ctx := testCtx(t)

	state, err := m.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	srv.CreateMailbox("Later")
	srv.Mail("Later", "in a new folder", "hello")

	ch, err := m.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(ch.Added) != 1 {
		t.Fatalf("added %v, want 1", ids(ch.Added))
	}
	if !ch.MailboxesChanged {
		t.Error("MailboxesChanged should be set when a folder appears")
	}
}

// \All duplicates the whole account under per-copy ids, so it stays out.
func TestAllMailIsExcludedByDefault(t *testing.T) {
	srv := imapfake.New(t)
	srv.CreateMailbox("All Mail", imapv2.MailboxAttrAll)
	srv.Mail("INBOX", "once", "hello")
	srv.Mail("All Mail", "once", "hello")

	m := newProvider(t, srv)
	page := enumerateAll(t, m, 50)
	for _, e := range page {
		if e.Mailboxes[0] == "All Mail" {
			t.Errorf("enumerated %q out of the all-mail folder", e.RemoteID)
		}
	}

	m2 := newProvider(t, srv, func(o *Options) { o.IncludeAllMail = true })
	page2 := enumerateAll(t, m2, 50)
	if len(page2) <= len(page) {
		t.Errorf("include_all_mail changed nothing: %d then %d", len(page), len(page2))
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
