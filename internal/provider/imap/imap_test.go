package imap

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/testutil/imapfake"
)

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// dial builds a provider pointed at a fake, with the folders a normal account
// has.
func dial(t *testing.T, opts ...imapfake.Option) (*Mail, *imapfake.Server) {
	t.Helper()
	return dialWith(t, opts...)
}

// dialWith is dial, spelled so a test reads as being about the capabilities it
// is hiding.
func dialWith(t *testing.T, opts ...imapfake.Option) (*Mail, *imapfake.Server) {
	t.Helper()
	srv := imapfake.New(t, opts...)
	srv.CreateMailbox("Archive", imapv2.MailboxAttrArchive)
	srv.CreateMailbox("Sent", imapv2.MailboxAttrSent)
	srv.CreateMailbox("Drafts", imapv2.MailboxAttrDrafts)
	srv.CreateMailbox("Trash", imapv2.MailboxAttrTrash)

	m := newProvider(t, srv)
	return m, srv
}

func newProvider(t *testing.T, srv *imapfake.Server, tweak ...func(*Options)) *Mail {
	t.Helper()
	host, port := splitAddr(t, srv.Addr())
	o := Options{
		Email:            imapfake.Username,
		Password:         imapfake.Password,
		Host:             host,
		Port:             port,
		Security:         SecNone,
		Insecure:         true,
		IncludeSpamTrash: true,
		Concurrency:      2,
		Logger:           testLogger(t),
	}
	for _, f := range tweak {
		f(&o)
	}
	m, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestMailboxesMapSpecialUseOntoRoles(t *testing.T) {
	m, _ := dial(t)

	boxes, err := m.Mailboxes(testCtx(t))
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	got := map[string]model.MailboxRole{}
	for _, b := range boxes {
		got[b.RemoteID] = b.Role
	}
	for name, want := range map[string]model.MailboxRole{
		"INBOX":   model.RoleInbox,
		"Archive": model.RoleArchive,
		"Sent":    model.RoleSent,
		"Drafts":  model.RoleDrafts,
		"Trash":   model.RoleTrash,
	} {
		if got[name] != want {
			t.Errorf("%s role = %q, want %q", name, got[name], want)
		}
	}
}

func TestEnumerateWalksEveryFolder(t *testing.T) {
	m, srv := dial(t)
	srv.Mail("INBOX", "first", "hello")
	srv.Mail("INBOX", "second", "hello again")
	srv.Mail("Archive", "old", "from the archive")

	ctx := testCtx(t)
	seen := map[string]bool{}
	cursor := ""
	for i := 0; ; i++ {
		if i > 20 {
			t.Fatal("Enumerate did not terminate")
		}
		page, next, err := m.Enumerate(ctx, cursor, 1)
		if err != nil {
			t.Fatalf("Enumerate: %v", err)
		}
		for _, e := range page {
			if seen[e.RemoteID] {
				t.Errorf("duplicate id %q", e.RemoteID)
			}
			seen[e.RemoteID] = true
			if len(e.Mailboxes) != 1 {
				t.Errorf("%s is in %d mailboxes, want exactly 1", e.RemoteID, len(e.Mailboxes))
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 3 {
		t.Fatalf("enumerated %d messages, want 3", len(seen))
	}
}

func TestFetchRawReturnsTheMessageAndDoesNotMarkItRead(t *testing.T) {
	m, srv := dial(t)
	srv.Mail("INBOX", "unread please", "body text")

	ctx := testCtx(t)
	page, _, err := m.Enumerate(ctx, "", 10)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("enumerated %d, want 1", len(page))
	}
	if !page[0].Flags.Unread {
		t.Error("a freshly appended message should be unread")
	}

	var got []provider.RawMessage
	err = m.FetchRaw(ctx, []string{page[0].RemoteID}, func(rm provider.RawMessage) error {
		got = append(got, rm)
		return nil
	})
	if err != nil {
		t.Fatalf("FetchRaw: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("fetched %d messages, want 1", len(got))
	}
	if !containsBytes(got[0].Raw, "body text") {
		t.Errorf("raw does not carry the body: %q", got[0].Raw)
	}
	if got[0].ThreadID == "" {
		t.Error("no thread id derived from the headers")
	}

	// BODY.PEEK, not RFC822: fetching must not silently mark the archive read.
	after, err := m.FetchEnvelopesSlice(ctx, []string{page[0].RemoteID})
	if err != nil {
		t.Fatalf("FetchEnvelopes: %v", err)
	}
	if len(after) != 1 || !after[0].Flags.Unread {
		t.Error("fetching the body marked the message read")
	}
}

func TestTotalCountsEverySyncedFolder(t *testing.T) {
	m, srv := dial(t)
	srv.Mail("INBOX", "a", "x")
	srv.Mail("Archive", "b", "x")

	n, err := m.Total(testCtx(t))
	if err != nil {
		t.Fatalf("Total: %v", err)
	}
	if n != 2 {
		t.Errorf("Total = %d, want 2", n)
	}
}

func TestNewRefusesPlaintextUnlessAsked(t *testing.T) {
	_, err := New(Options{
		Email: "a@example.com", Password: "x", Host: "localhost", Security: SecNone,
	})
	if err == nil {
		t.Fatal("New allowed a cleartext password without Insecure")
	}
}

func TestNewNeedsAHostWithoutAPreset(t *testing.T) {
	_, err := New(Options{Email: "a@example.com", Password: "x"})
	if err == nil {
		t.Fatal("New accepted an account with no host and no vendor preset")
	}
}

func TestPresetSuppliesTheHost(t *testing.T) {
	m, err := New(Options{Email: "a@icloud.com", Password: "x", Vendor: model.VendorICloud})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.addr != "imap.mail.me.com:993" {
		t.Errorf("addr = %q, want the iCloud preset", m.addr)
	}
	if m.smtpAddr != "smtp.mail.me.com:587" {
		t.Errorf("smtpAddr = %q, want the iCloud preset", m.smtpAddr)
	}
	// Apple locks an account out for too many connections; the preset's cap
	// must win over a larger configured concurrency.
	m2, _ := New(Options{Email: "a@icloud.com", Password: "x", Vendor: model.VendorICloud, Concurrency: 32})
	if got := m2.maxConns(); got != 4 {
		t.Errorf("maxConns = %d, want the preset cap of 4", got)
	}
}

// enumerateAll drains Enumerate. It pages per folder, so a single call only
// ever covers one of them.
func enumerateAll(t *testing.T, m *Mail, limit int) []provider.Envelope {
	t.Helper()
	ctx := testCtx(t)
	var out []provider.Envelope
	cursor := ""
	for i := 0; ; i++ {
		if i > 200 {
			t.Fatal("Enumerate did not terminate")
		}
		page, next, err := m.Enumerate(ctx, cursor, limit)
		if err != nil {
			t.Fatalf("Enumerate: %v", err)
		}
		out = append(out, page...)
		if next == "" {
			return out
		}
		cursor = next
	}
}

// FetchEnvelopesSlice is a test convenience over FetchEnvelopes.
func (m *Mail) FetchEnvelopesSlice(ctx context.Context, ids []string) ([]provider.Envelope, error) {
	var out []provider.Envelope
	err := m.FetchEnvelopes(ctx, ids, func(e provider.Envelope) error {
		out = append(out, e)
		return nil
	})
	return out, err
}

func containsBytes(b []byte, want string) bool {
	return len(b) > 0 && strings.Contains(string(b), want)
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("bad addr %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port in %q: %v", addr, err)
	}
	return host, port
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
