package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

// mail is a message for the address book tests: who it is from and to, and
// which mailbox it sits in ("" for none).
func mail(account, remote, mailbox string, from model.Address, to []model.Address, when time.Time) *model.Message {
	m := &model.Message{
		AccountID: account, RemoteID: remote, ThreadID: "t-" + remote,
		Subject: "about " + remote, From: from, To: to, Date: when, Received: when,
	}
	if mailbox != "" {
		m.MailboxRemotes = []string{mailbox}
	}
	return m
}

func contactsOf(t *testing.T, s *Store, f ContactFilter) []model.Contact {
	t.Helper()
	out, err := s.SearchContacts(context.Background(), f)
	if err != nil {
		t.Fatalf("SearchContacts: %v", err)
	}
	return out
}

func emailsOf(cs []model.Contact) string {
	var e []string
	for _, c := range cs {
		e = append(e, c.Email)
	}
	return strings.Join(e, " ")
}

func TestContactsCollectEveryAddressOnAMessage(t *testing.T) {
	s := newTestStore(t)
	seedAccount(t, s, "work")
	m := mail("work", "m1", "mb-inbox", addr("Anna de Vries", "Anna@Example.com"),
		[]model.Address{{Email: "work@example.com"}, {Name: "Bob", Email: "bob@example.com"}}, base)
	m.Cc = []model.Address{{Email: "carol@example.com"}}
	m.Bcc = []model.Address{{Email: "dave@example.com"}}
	m.ReplyTo = []model.Address{{Email: "list@example.com"}}
	putMessage(t, s, m, nil)

	got := contactsOf(t, s, ContactFilter{})
	if want := "anna@example.com bob@example.com carol@example.com dave@example.com"; emailsOf(got) != want {
		t.Fatalf("contacts = %q, want %q", emailsOf(got), want)
	}
	if got[0].Name != "Anna de Vries" || got[1].Name != "Bob" || got[2].Name != "" {
		t.Errorf("names = %q %q %q", got[0].Name, got[1].Name, got[2].Name)
	}
	if got[0].Count != 1 || got[0].SentCount != 0 || !got[0].Last.Equal(base) {
		t.Errorf("anna = %+v", got[0])
	}
	if len(got[0].Accounts) != 1 || got[0].Accounts[0] != "work" {
		t.Errorf("anna.Accounts = %v", got[0].Accounts)
	}
}

func TestContactsRankWhoYouWriteToAheadOfWhoWritesToYou(t *testing.T) {
	s := newTestStore(t)
	seedAccount(t, s, "work")
	me := addr("", "work@example.com")
	for i := range 5 {
		putMessage(t, s, mail("work", "in"+string(rune('a'+i)), "mb-inbox",
			addr("Yara", "yara@example.com"), []model.Address{me}, base.Add(time.Duration(i)*time.Hour)), nil)
	}
	// One in the sent mailbox, and one that only has the account's own
	// address on From, the way a Gmail "All Mail" row or a reindexed draft
	// arrives.
	putMessage(t, s, mail("work", "s1", "mb-sent", me, []model.Address{addr("Xavier", "xavier@example.com")}, base.Add(-48*time.Hour)), nil)
	putMessage(t, s, mail("work", "s2", "", me, []model.Address{addr("", "zed@example.com")}, base.Add(-72*time.Hour)), nil)
	d := mail("work", "d1", "", addr("", "other@example.com"), []model.Address{addr("", "zed@example.com")}, base.Add(-96*time.Hour))
	d.Flags.Draft = true
	putMessage(t, s, d, nil)

	got := contactsOf(t, s, ContactFilter{})
	if want := "zed@example.com xavier@example.com yara@example.com other@example.com"; emailsOf(got) != want {
		t.Fatalf("contacts = %q, want %q", emailsOf(got), want)
	}
	if got[0].SentCount != 2 || got[1].SentCount != 1 || got[2].SentCount != 0 || got[2].Count != 5 {
		t.Errorf("counts: zed %d/%d xavier %d/%d yara %d/%d",
			got[0].SentCount, got[0].Count, got[1].SentCount, got[1].Count, got[2].SentCount, got[2].Count)
	}
}

func TestContactsLeaveOutYourOwnAddresses(t *testing.T) {
	s := newTestStore(t)
	seedAccount(t, s, "work")
	putMessage(t, s, mail("work", "s1", "mb-sent", addr("Me", "Work@example.com"),
		[]model.Address{{Email: "work@example.com"}, {Email: "anna@example.com"}}, base), nil)
	if got := contactsOf(t, s, ContactFilter{}); emailsOf(got) != "anna@example.com" {
		t.Errorf("contacts = %q, want just anna", emailsOf(got))
	}
}

func TestContactsLeaveOutTheRobots(t *testing.T) {
	s := newTestStore(t)
	seedAccount(t, s, "work")
	me := []model.Address{{Email: "work@example.com"}}
	for i, from := range []string{
		"noreply@shop.example", "no-reply@bank.example", "notifications-abc@github.example",
		"alerts@monitor.example", "MAILER-DAEMON@mx.example", "bounce+xyz@list.example",
		"anna@example.com",
	} {
		putMessage(t, s, mail("work", "m"+string(rune('a'+i)), "mb-inbox", addr("", from), me, base), nil)
	}
	if got := contactsOf(t, s, ContactFilter{}); emailsOf(got) != "anna@example.com" {
		t.Errorf("contacts = %q, want just anna", emailsOf(got))
	}
}

func TestContactsDoNotDoubleCountAReindexedMessage(t *testing.T) {
	s := newTestStore(t)
	seedAccount(t, s, "work")
	m := mail("work", "s1", "mb-sent", addr("", "work@example.com"), []model.Address{{Email: "bob@example.com"}}, base)
	putMessage(t, s, m, nil)
	putMessage(t, s, m, nil)
	got := contactsOf(t, s, ContactFilter{})
	if emailsOf(got) != "bob@example.com" || got[0].Count != 1 || got[0].SentCount != 1 {
		t.Fatalf("after two upserts: %+v", got)
	}

	// The same remote id, now addressed to somebody else: bob is forgotten,
	// dave is known.
	m.To = []model.Address{{Email: "dave@example.com"}}
	putMessage(t, s, m, nil)
	if got := contactsOf(t, s, ContactFilter{}); emailsOf(got) != "dave@example.com" {
		t.Errorf("after the rewrite: %q, want just dave", emailsOf(got))
	}
}

func TestContactsMatchTheNameOrTheAddress(t *testing.T) {
	s := newTestStore(t)
	seedAccount(t, s, "work")
	me := []model.Address{{Email: "work@example.com"}}
	putMessage(t, s, mail("work", "m1", "mb-inbox", addr("Anna de Vries", "anna@example.com"), me, base), nil)
	putMessage(t, s, mail("work", "m2", "mb-inbox", addr("Bob", "bob@other.example"), me, base), nil)
	for q, want := range map[string]string{
		"VRIES": "anna@example.com", "bob@": "bob@other.example", "example": "anna@example.com bob@other.example",
		"%": "", "zzz": "",
	} {
		if got := contactsOf(t, s, ContactFilter{Query: q}); emailsOf(got) != want {
			t.Errorf("query %q = %q, want %q", q, emailsOf(got), want)
		}
	}
	if got := contactsOf(t, s, ContactFilter{Limit: 1}); emailsOf(got) != "anna@example.com" {
		t.Errorf("limit 1 = %q", emailsOf(got))
	}
}

func TestContactsMergeAcrossAccountsAndNarrowToOne(t *testing.T) {
	s := newTestStore(t)
	seedAccount(t, s, "work")
	seedAccount(t, s, "home")
	anna := addr("Anna de Vries", "anna@example.com")
	putMessage(t, s, mail("work", "m1", "mb-inbox", anna, []model.Address{{Email: "work@example.com"}}, base), nil)
	putMessage(t, s, mail("home", "m1", "mb-inbox", addr("anna", "anna@example.com"), []model.Address{{Email: "home@example.com"}}, base.Add(time.Hour)), nil)
	// Each account's own address is left out of the other's book too.
	putMessage(t, s, mail("home", "m2", "mb-inbox", addr("", "work@example.com"), []model.Address{{Email: "home@example.com"}}, base), nil)

	got := contactsOf(t, s, ContactFilter{})
	if len(got) != 1 || got[0].Count != 2 || strings.Join(got[0].Accounts, ",") != "home,work" {
		t.Fatalf("merged = %+v", got)
	}
	if got[0].Name != "anna" || !got[0].Last.Equal(base.Add(time.Hour)) {
		t.Errorf("the newest account's name and date win: %+v", got[0])
	}
	got = contactsOf(t, s, ContactFilter{Accounts: []string{"work"}})
	if len(got) != 1 || got[0].Count != 1 || got[0].Name != "Anna de Vries" || len(got[0].Accounts) != 1 {
		t.Errorf("work only = %+v", got)
	}
}

func TestContactsPreferTheNameSomebodyGivesThemself(t *testing.T) {
	s := newTestStore(t)
	seedAccount(t, s, "work")
	me := addr("", "work@example.com")
	putMessage(t, s, mail("work", "s1", "mb-sent", me, []model.Address{addr("anna", "anna@example.com")}, base.Add(time.Hour)), nil)
	putMessage(t, s, mail("work", "m1", "mb-inbox", addr("Anna de Vries", "anna@example.com"), []model.Address{me}, base), nil)
	if got := contactsOf(t, s, ContactFilter{}); len(got) != 1 || got[0].Name != "Anna de Vries" {
		t.Errorf("contacts = %+v", got)
	}
}

func TestContactsAreBackfilledByTheMigration(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s, "work")
	me := addr("", "work@example.com")
	putMessage(t, s, mail("work", "s1", "mb-sent", me, []model.Address{addr("Bob", "bob@example.com")}, base), nil)
	m := mail("work", "m1", "mb-inbox", addr("Anna de Vries", "anna@example.com"), []model.Address{me}, base)
	m.Cc = []model.Address{{Email: "carol@example.com"}}
	putMessage(t, s, m, nil)
	want := contactsOf(t, s, ContactFilter{})

	for _, stmt := range []string{
		`DROP TABLE contacts`, `DROP TABLE message_addresses`,
		`DELETE FROM schema_migrations WHERE version = 6`,
	} {
		if _, err := s.DB().ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	got := contactsOf(t, s, ContactFilter{})
	if emailsOf(got) != emailsOf(want) || len(got) != 3 {
		t.Fatalf("after the backfill: %q, want %q", emailsOf(got), emailsOf(want))
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].SentCount != want[i].SentCount || got[i].Count != want[i].Count || !got[i].Last.Equal(want[i].Last) {
			t.Errorf("%s: backfill %+v, indexing %+v", want[i].Email, got[i], want[i])
		}
	}
}

func TestRebuildContactsForgetsWhatNoMessageNamesAnyMore(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s, "work")
	me := addr("", "work@example.com")
	putMessage(t, s, mail("work", "s1", "mb-sent", me, []model.Address{{Email: "bob@example.com"}}, base), nil)
	putMessage(t, s, mail("work", "s2", "mb-sent", me, []model.Address{{Email: "anna@example.com"}}, base), nil)

	// A purge: the row leaves the messages table behind the store's back.
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM messages WHERE remote_id = 's1'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RebuildContacts(ctx, "work"); err != nil {
		t.Fatalf("RebuildContacts: %v", err)
	}
	if got := contactsOf(t, s, ContactFilter{}); emailsOf(got) != "anna@example.com" {
		t.Errorf("after the rebuild: %q, want just anna", emailsOf(got))
	}
}
