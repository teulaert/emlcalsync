package imap

import (
	"strings"
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/testutil/imapfake"
)

// These pin the things this package must never do. Each one is cheap to
// reintroduce while "simplifying" something nearby, and expensive to notice:
// they do not fail, they quietly corrupt an archive or somebody's mailbox.

// go-imap/v2 has no QRESYNC. VANISHED is absent from the untagged-response
// switch, whose default branch is an error that tears the connection down — so
// enabling it would not degrade, it would break the account outright the first
// time a message was expunged elsewhere.
func TestNeverEnablesQRESYNC(t *testing.T) {
	srv := imapfake.New(t, imapfake.WithCaps(
		imapv2.CapIMAP4rev1, imapv2.CapUIDPlus, imapv2.CapMove,
		imapv2.CapCondStore, imapv2.CapQResync, imapv2.CapESearch,
	), imapfake.Record())
	srv.CreateMailbox("Archive")
	m := newProvider(t, srv)
	srv.Mail("INBOX", "hello", "body")

	if _, err := m.State(testCtx(t)); err != nil {
		t.Fatalf("State: %v", err)
	}
	if srv.Sent("QRESYNC") {
		t.Errorf("the client enabled or requested QRESYNC:\n%s", srv.Transcript())
	}
}

// Every FETCH item has to be one the client's own parser knows: an unknown
// msg-att name is a hard error in go-imap, not a skipped field. That rules out
// EMAILID and the X-GM-* extensions, however tempting.
func TestNeverRequestsUnparseableFetchItems(t *testing.T) {
	srv := imapfake.New(t, imapfake.WithCaps(
		imapv2.CapIMAP4rev1, imapv2.CapUIDPlus, imapv2.CapObjectID, imapv2.CapESearch,
	), imapfake.Record())
	m := newProvider(t, srv)
	srv.Mail("INBOX", "hello", "body")

	page := enumerateAll(t, m, 10)
	if err := m.FetchRaw(testCtx(t), []string{page[0].RemoteID}, func(rm provider.RawMessage) error {
		return nil
	}); err != nil {
		t.Fatalf("FetchRaw: %v", err)
	}
	for _, forbidden := range []string{"EMAILID", "THREADID", "X-GM-"} {
		if srv.Sent(forbidden) {
			t.Errorf("client requested %s, which its own parser cannot read:\n%s",
				forbidden, srv.Transcript())
		}
	}
}

// A bare EXPUNGE removes every \Deleted message in the folder, including ones
// another client flagged and has not committed to. Without UIDPLUS to scope it,
// the source copy stays and the tombstone is left behind on purpose.
func TestNeverSendsABareExpunge(t *testing.T) {
	srv := imapfake.New(t,
		imapfake.HideCaps(imapv2.CapMove, imapv2.CapUIDPlus),
		imapfake.Record())
	srv.CreateMailbox("Archive")
	m := newProvider(t, srv)
	srv.Mail("INBOX", "moving", "body")

	id := enumerateAll(t, m, 10)[0].RemoteID
	if _, err := m.SetMailboxesRemap(testCtx(t), []string{id}, []string{"Archive"}, []string{"INBOX"}); err != nil {
		t.Fatalf("SetMailboxesRemap: %v", err)
	}

	for _, line := range strings.Split(srv.Transcript(), "\n") {
		up := strings.ToUpper(strings.TrimSpace(line))
		// "UID EXPUNGE 3" is scoped and fine; a bare "EXPUNGE" is not.
		if fields := strings.Fields(up); len(fields) >= 2 &&
			fields[1] == "EXPUNGE" && !strings.Contains(up, "UID EXPUNGE") {
			t.Errorf("client sent a bare EXPUNGE, which would delete other clients' work:\n%s", line)
		}
	}
}

// RFC822 sets \Seen as a side effect. Using it instead of BODY.PEEK[] would
// mark a user's entire archive read on the first backfill, silently and
// irreversibly.
func TestFetchAlwaysPeeks(t *testing.T) {
	srv := imapfake.New(t, imapfake.Record())
	m := newProvider(t, srv)
	srv.Mail("INBOX", "still unread afterwards", "body")

	page := enumerateAll(t, m, 10)
	if err := m.FetchRaw(testCtx(t), []string{page[0].RemoteID}, func(provider.RawMessage) error {
		return nil
	}); err != nil {
		t.Fatalf("FetchRaw: %v", err)
	}
	if !srv.Sent("BODY.PEEK") {
		t.Errorf("no BODY.PEEK in the conversation:\n%s", srv.Transcript())
	}
	after, err := m.FetchEnvelopesSlice(testCtx(t), []string{page[0].RemoteID})
	if err != nil {
		t.Fatalf("FetchEnvelopes: %v", err)
	}
	if !after[0].Flags.Unread {
		t.Error("fetching the body marked the message read")
	}
}

// A read path must never open a mailbox read-write: SELECT without EXAMINE can
// expunge \Deleted messages on close, on some servers.
func TestReadPathsSelectReadOnly(t *testing.T) {
	srv := imapfake.New(t, imapfake.Record())
	m := newProvider(t, srv)
	srv.Mail("INBOX", "just reading", "body")

	enumerateAll(t, m, 10)
	if !srv.Sent("EXAMINE") {
		t.Errorf("no read-only SELECT in the conversation:\n%s", srv.Transcript())
	}
}

// \Flagged is a virtual starred view, not a folder. Mapping it to a role the
// engine files into would have archive write somewhere that holds nothing.
func TestFlaggedIsNeverARole(t *testing.T) {
	m := &Mail{}
	if got := m.roleFor("Starred", '/', []imapv2.MailboxAttr{imapv2.MailboxAttrFlagged}); got != "" {
		t.Errorf("\\Flagged mapped to %q", got)
	}
}

// A UIDVALIDITY roll must be reported precisely, not by expiring the state:
// expiring makes the engine reconcile the whole account over one folder.
func TestUIDValidityResetNeverExpiresTheState(t *testing.T) {
	m, srv := dial(t)
	ctx := testCtx(t)
	srv.CreateMailbox("Volatile")
	srv.Mail("Volatile", "before", "hello")

	state, err := m.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	srv.DeleteMailbox("Volatile")
	srv.CreateMailbox("Volatile")
	srv.Mail("Volatile", "after", "hello")

	if _, err := m.Changes(ctx, state); err != nil {
		t.Fatalf("a uidvalidity reset expired the state: %v", err)
	}
}

// Flags must round-trip through the model without inverting twice.
func TestFlagMappingRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		imap []imapv2.Flag
		want model.Flags
	}{
		{nil, model.Flags{Unread: true}},
		{[]imapv2.Flag{imapv2.FlagSeen}, model.Flags{}},
		{[]imapv2.Flag{imapv2.FlagSeen, imapv2.FlagFlagged}, model.Flags{Flagged: true}},
		{[]imapv2.Flag{imapv2.FlagAnswered}, model.Flags{Unread: true, Answered: true}},
		{[]imapv2.Flag{imapv2.FlagDraft}, model.Flags{Unread: true, Draft: true}},
	} {
		if got := flagsFrom(tc.imap); got != tc.want {
			t.Errorf("flagsFrom(%v) = %+v, want %+v", tc.imap, got, tc.want)
		}
	}
}
