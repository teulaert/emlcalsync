package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/lennert/emlcal/internal/mime"
	"github.com/lennert/emlcal/internal/model"
)

// base is the reference time every fixture hangs off.
var base = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// newTestStore opens a real file-backed database so the tests exercise WAL,
// the busy timeout and the connection pool the way production does.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.SetLogger(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { s.Close() })
	return s
}

func seedAccount(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertAccount(ctx, &model.Account{
		ID: id, Provider: model.ProviderFastmail, Email: id + "@example.com", CreatedAt: base,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if err := s.ReplaceMailboxes(ctx, id, []model.Mailbox{
		{RemoteID: "mb-inbox", Name: "Inbox", Role: model.RoleInbox, SortOrder: 1},
		{RemoteID: "mb-archive", Name: "Archive", Role: model.RoleArchive, SortOrder: 2},
		{RemoteID: "mb-sent", Name: "Sent", Role: model.RoleSent, SortOrder: 3},
		{RemoteID: "mb-work", Name: "Work", SortOrder: 4},
		{RemoteID: "mb-work-sub", Name: "Clients", ParentRemote: "mb-work", SortOrder: 5},
	}); err != nil {
		t.Fatalf("ReplaceMailboxes: %v", err)
	}
}

// putMessage indexes one message and returns its row id.
func putMessage(t *testing.T, s *Store, m *model.Message, p *mime.Parsed) int64 {
	t.Helper()
	id, err := s.UpsertMessage(context.Background(), m, p)
	if err != nil {
		t.Fatalf("UpsertMessage %s: %v", m.RemoteID, err)
	}
	return id
}

func addr(name, email string) model.Address { return model.Address{Name: name, Email: email} }

func ptrBool(b bool) *bool { return &b }

// ---------------------------------------------------------------------------

func TestOpenAndMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	v1, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v1 < 1 {
		t.Fatalf("schema version = %d, want >= 1", v1)
	}
	seedAccount(t, s, "work")

	// Re-running the migrator on an already-migrated database is a no-op.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening applies nothing new and keeps the data.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	v2, err := s2.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v2 != v1 {
		t.Fatalf("schema version changed on reopen: %d -> %d", v1, v2)
	}
	if a, err := s2.GetAccount(ctx, "work"); err != nil || a.Email != "work@example.com" {
		t.Fatalf("account lost across reopen: %+v %v", a, err)
	}

	var mode string
	if err := s2.DB().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	var fk int
	if err := s2.DB().QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatal("foreign_keys not enabled")
	}
}

func TestOpenMemory(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.UpsertAccount(ctx, &model.Account{ID: "m", Provider: model.ProviderGmail, Email: "m@x"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	// The pool is pinned to one connection, so the schema survives.
	for range 5 {
		if _, err := s.ListAccounts(ctx); err != nil {
			t.Fatalf("ListAccounts: %v", err)
		}
	}
	// Two in-memory stores must not share a database.
	s2, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if accts, err := s2.ListAccounts(ctx); err != nil || len(accts) != 0 {
		t.Fatalf("second :memory: store sees %v (%v), want empty", accts, err)
	}
}

func TestAccountsCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.GetAccount(ctx, "nope"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetAccount(missing) = %v, want ErrNotFound", err)
	}
	if err := s.UpsertAccount(ctx, &model.Account{ID: "Bad Name", Provider: model.ProviderGmail}); err == nil {
		t.Fatal("invalid account id accepted")
	}

	a := &model.Account{ID: "work", Provider: model.ProviderGmail, Email: "a@b.c"}
	if err := s.UpsertAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	if a.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not filled in")
	}
	created := a.CreatedAt

	// Update keeps created_at.
	a2 := &model.Account{ID: "work", Provider: model.ProviderGmail, Email: "changed@b.c"}
	if err := s.UpsertAccount(ctx, a2); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "changed@b.c" {
		t.Fatalf("email = %q", got.Email)
	}
	if !got.CreatedAt.Equal(created.Truncate(time.Second)) {
		t.Fatalf("created_at changed: %v -> %v", created, got.CreatedAt)
	}

	if err := s.UpsertAccount(ctx, &model.Account{ID: "personal", Provider: model.ProviderFastmail, Email: "p@x"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "personal" || list[1].ID != "work" {
		t.Fatalf("ListAccounts = %+v", list)
	}
}

func TestDeleteAccountCascades(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")
	seedAccount(t, s, "keep")

	for _, acct := range []string{"work", "keep"} {
		putMessage(t, s, &model.Message{
			AccountID: acct, RemoteID: "m1", ThreadID: "t1", BlobSHA256: "sha-" + acct,
			RawComplete: true, Subject: "hello", From: addr("A", "a@example.com"),
			Received: base, MailboxRemotes: []string{"mb-inbox"},
		}, &mime.Parsed{
			TextBody:    "cascade test body",
			Attachments: []mime.Part{{Path: "2", Filename: "a.pdf", ContentType: "application/pdf", Size: 10}},
		})
		if _, err := s.EnqueueOutbox(ctx, acct, "flags", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		if err := s.SetState(ctx, acct, "mail", "state-1"); err != nil {
			t.Fatal(err)
		}
		if err := s.ReplaceCalendars(ctx, acct, []model.Calendar{{RemoteID: "cal", Name: "Cal"}}); err != nil {
			t.Fatal(err)
		}
		ev := &model.Event{AccountID: acct, CalendarRemote: "cal", RemoteID: "ev1", Title: "x",
			Start: base, End: base.Add(time.Hour), RawJSON: []byte(`{}`)}
		if _, err := s.UpsertEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
		if err := s.ReplaceOccurrences(ctx, ev.ID, []model.Occurrence{{EventID: ev.ID, Start: base, End: base.Add(time.Hour)}}); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.DeleteAccount(ctx, "work"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	for _, tc := range []struct{ name, query string }{
		{"messages", `SELECT count(*) FROM messages WHERE account_id = 'work'`},
		{"mailboxes", `SELECT count(*) FROM mailboxes WHERE account_id = 'work'`},
		{"threads", `SELECT count(*) FROM threads WHERE account_id = 'work'`},
		{"sync_state", `SELECT count(*) FROM sync_state WHERE account_id = 'work'`},
		{"outbox", `SELECT count(*) FROM outbox WHERE account_id = 'work'`},
		{"calendars", `SELECT count(*) FROM calendars WHERE account_id = 'work'`},
		{"accounts", `SELECT count(*) FROM accounts WHERE id = 'work'`},
		{"attachments", `SELECT count(*) FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.account_id = 'work'`},
		{"memberships", `SELECT count(*) FROM message_mailboxes mm JOIN messages m ON m.id = mm.message_id WHERE m.account_id = 'work'`},
		{"events", `SELECT count(*) FROM events e JOIN calendars c ON c.id = e.calendar_id WHERE c.account_id = 'work'`},
	} {
		var n int
		if err := s.DB().QueryRowContext(ctx, tc.query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if n != 0 {
			t.Errorf("%s: %d rows left after DeleteAccount", tc.name, n)
		}
	}

	// The FTS index must not be left holding entries for deleted rows.
	var ftsRows int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM messages_fts`).Scan(&ftsRows); err != nil {
		t.Fatal(err)
	}
	if ftsRows != 1 {
		t.Fatalf("messages_fts holds %d rows, want 1 (the surviving account)", ftsRows)
	}
	if report, err := s.IntegrityCheck(ctx); err != nil || report != "ok" {
		t.Fatalf("IntegrityCheck = %q, %v", report, err)
	}

	// The other account is untouched.
	if _, err := s.GetMessage(ctx, "keep", "m1"); err != nil {
		t.Fatalf("surviving account lost its message: %v", err)
	}
}

// ---------------------------------------------------------------------------

func TestReplaceMailboxes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	mbs, err := s.ListMailboxes(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(mbs) != 5 {
		t.Fatalf("got %d mailboxes, want 5", len(mbs))
	}
	var sub model.Mailbox
	for _, m := range mbs {
		if m.RemoteID == "mb-work-sub" {
			sub = m
		}
	}
	if sub.ParentRemote != "mb-work" {
		t.Fatalf("ParentRemote = %q, want mb-work", sub.ParentRemote)
	}
	if sub.ID == 0 {
		t.Fatal("mailbox id not set")
	}

	// A message in a mailbox that later disappears loses the membership.
	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "m1", ThreadID: "t1", Received: base,
		MailboxRemotes: []string{"mb-inbox", "mb-work-sub"},
	}, nil)

	// Replace: rename Inbox, drop the sub-mailbox, add a new one.
	if err := s.ReplaceMailboxes(ctx, "work", []model.Mailbox{
		{RemoteID: "mb-inbox", Name: "Postvak IN", Role: model.RoleInbox, SortOrder: 1, TotalCount: 42, UnreadCount: 7},
		{RemoteID: "mb-archive", Name: "Archive", Role: model.RoleArchive, SortOrder: 2},
		{RemoteID: "mb-sent", Name: "Sent", Role: model.RoleSent, SortOrder: 3},
		{RemoteID: "mb-work", Name: "Work", SortOrder: 4},
		{RemoteID: "mb-new", Name: "Newsletters", SortOrder: 9},
	}); err != nil {
		t.Fatalf("ReplaceMailboxes: %v", err)
	}

	mbs, err = s.ListMailboxes(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(mbs) != 5 {
		t.Fatalf("after replace: %d mailboxes, want 5", len(mbs))
	}
	byRemote := map[string]model.Mailbox{}
	for _, m := range mbs {
		byRemote[m.RemoteID] = m
	}
	if _, gone := byRemote["mb-work-sub"]; gone {
		t.Fatal("removed mailbox still present")
	}
	if byRemote["mb-inbox"].Name != "Postvak IN" || byRemote["mb-inbox"].UnreadCount != 7 {
		t.Fatalf("inbox not updated: %+v", byRemote["mb-inbox"])
	}
	if _, ok := byRemote["mb-new"]; !ok {
		t.Fatal("new mailbox not inserted")
	}

	msg, err := s.GetMessage(ctx, "work", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.MailboxRemotes) != 1 || msg.MailboxRemotes[0] != "mb-inbox" {
		t.Fatalf("membership after mailbox removal = %v, want [mb-inbox]", msg.MailboxRemotes)
	}
}

func TestFindMailbox(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	for _, tc := range []struct{ query, want string }{
		{"inbox", "mb-inbox"},
		{"INBOX", "mb-inbox"},
		{"Archive", "mb-archive"},
		{"work", "mb-work"},       // name match
		{"client", "mb-work-sub"}, // unique prefix
	} {
		mb, err := s.FindMailbox(ctx, "work", tc.query)
		if err != nil {
			t.Fatalf("FindMailbox(%q): %v", tc.query, err)
		}
		if mb.RemoteID != tc.want {
			t.Errorf("FindMailbox(%q) = %s, want %s", tc.query, mb.RemoteID, tc.want)
		}
	}

	if _, err := s.FindMailbox(ctx, "work", "does-not-exist"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("FindMailbox(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.GetMailboxByRemote(ctx, "work", "nope"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetMailboxByRemote(missing) = %v, want ErrNotFound", err)
	}
	mb, err := s.GetMailboxByRemote(ctx, "work", "mb-work-sub")
	if err != nil {
		t.Fatal(err)
	}
	if mb.ParentRemote != "mb-work" {
		t.Fatalf("ParentRemote = %q", mb.ParentRemote)
	}
}

// ---------------------------------------------------------------------------

func TestUpsertMessageRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	parsed := &mime.Parsed{
		MessageID:  "abc@example.com",
		InReplyTo:  "parent@example.com",
		References: []string{"root@example.com", "parent@example.com"},
		Subject:    "Quarterly report",
		From:       addr("Alice Example", "alice@example.com"),
		To:         []model.Address{addr("Bob", "bob@example.com")},
		Cc:         []model.Address{addr("", "carol@example.com")},
		Date:       base.Add(-time.Hour),
		TextBody:   "Please find the quarterly   report attached.\n\nRegards,\nAlice",
		HasHTML:    true,
		HTMLPart:   "1.2",
		ListID:     "",
		Attachments: []mime.Part{
			{Path: "2", Filename: "report.pdf", ContentType: "application/pdf", Size: 12345, Disposition: "attachment"},
			{Path: "3", Filename: "logo.png", ContentType: "image/png", Size: 900, ContentID: "logo1", Inline: true},
		},
	}
	msg := &model.Message{
		AccountID: "work", RemoteID: "r-1", ThreadID: "th-1",
		BlobSHA256: "0123456789abcdef", RawComplete: true,
		Received: base, Size: 54321,
		Flags:          model.Flags{Unread: true},
		MailboxRemotes: []string{"mb-inbox", "mb-work"},
	}
	id := putMessage(t, s, msg, parsed)
	if id == 0 || msg.ID != id {
		t.Fatalf("row id not returned/written back: %d %d", id, msg.ID)
	}

	got, err := s.GetMessage(ctx, "work", "r-1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Subject != "Quarterly report" {
		t.Errorf("Subject = %q", got.Subject)
	}
	if got.From.Email != "alice@example.com" || got.From.Name != "Alice Example" {
		t.Errorf("From = %+v", got.From)
	}
	if len(got.To) != 1 || got.To[0].Email != "bob@example.com" {
		t.Errorf("To = %+v", got.To)
	}
	if len(got.Cc) != 1 || got.Cc[0].Email != "carol@example.com" {
		t.Errorf("Cc = %+v", got.Cc)
	}
	if got.MessageIDHeader != "abc@example.com" || got.InReplyTo != "parent@example.com" {
		t.Errorf("message id headers = %q / %q", got.MessageIDHeader, got.InReplyTo)
	}
	if len(got.References) != 2 {
		t.Errorf("References = %v", got.References)
	}
	if !got.Date.Equal(base.Add(-time.Hour)) || !got.Received.Equal(base) {
		t.Errorf("times = %v / %v", got.Date, got.Received)
	}
	if got.TextBody != parsed.TextBody {
		t.Errorf("TextBody = %q", got.TextBody)
	}
	if got.Snippet == "" || len(got.Snippet) > 220 {
		t.Errorf("Snippet = %q", got.Snippet)
	}
	if want := "Please find the quarterly report attached."; got.Snippet[:len(want)] != want {
		t.Errorf("snippet whitespace not collapsed: %q", got.Snippet)
	}
	if !got.HasAttachments {
		t.Error("HasAttachments = false")
	}
	if !got.Flags.Unread || got.Flags.Flagged {
		t.Errorf("Flags = %+v", got.Flags)
	}
	if !got.RawComplete || got.BlobSHA256 != "0123456789abcdef" {
		t.Errorf("blob = %q complete=%v", got.BlobSHA256, got.RawComplete)
	}
	if got.DeletedAt != nil {
		t.Error("DeletedAt set")
	}
	if len(got.MailboxRemotes) != 2 {
		t.Errorf("MailboxRemotes = %v", got.MailboxRemotes)
	}
	if got.PublicID() != "work:r-1" {
		t.Errorf("PublicID = %s", got.PublicID())
	}

	atts, err := s.ListAttachments(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 2 {
		t.Fatalf("%d attachments, want 2", len(atts))
	}
	if atts[0].Filename != "report.pdf" || atts[0].Size != 12345 || atts[0].PartPath != "2" {
		t.Errorf("attachment 0 = %+v", atts[0])
	}
	if !atts[1].Inline || atts[1].ContentID != "logo1" {
		t.Errorf("attachment 1 = %+v", atts[1])
	}
	if err := s.SetAttachmentRemoteRef(ctx, atts[0].ID, "blob-ref-1"); err != nil {
		t.Fatal(err)
	}
	atts, _ = s.ListAttachments(ctx, id)
	if atts[0].RemoteRef != "blob-ref-1" {
		t.Errorf("RemoteRef = %q", atts[0].RemoteRef)
	}

	byID, err := s.GetMessageByID(ctx, id)
	if err != nil || byID.RemoteID != "r-1" {
		t.Fatalf("GetMessageByID = %+v %v", byID, err)
	}

	// Re-upserting the same remote id updates in place, replacing derived rows.
	parsed.Subject = "Quarterly report (revised)"
	parsed.Attachments = parsed.Attachments[:1]
	msg2 := &model.Message{
		AccountID: "work", RemoteID: "r-1", ThreadID: "th-1", BlobSHA256: "0123456789abcdef",
		RawComplete: true, Received: base, MailboxRemotes: []string{"mb-archive"},
	}
	id2 := putMessage(t, s, msg2, parsed)
	if id2 != id {
		t.Fatalf("upsert created a second row: %d != %d", id2, id)
	}
	got, err = s.GetMessage(ctx, "work", "r-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "Quarterly report (revised)" {
		t.Errorf("subject not updated: %q", got.Subject)
	}
	if len(got.MailboxRemotes) != 1 || got.MailboxRemotes[0] != "mb-archive" {
		t.Errorf("memberships not replaced: %v", got.MailboxRemotes)
	}
	atts, _ = s.ListAttachments(ctx, id)
	if len(atts) != 1 {
		t.Errorf("attachments not replaced: %d", len(atts))
	}

	var count int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM messages`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("%d message rows, want 1", count)
	}

	if _, err := s.GetMessage(ctx, "work", "missing"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetMessage(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.GetMessageByID(ctx, 9999); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetMessageByID(missing) = %v, want ErrNotFound", err)
	}
}

func TestUpsertMessageUnknownMailboxIsIgnored(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "r-1", ThreadID: "t", Received: base,
		MailboxRemotes: []string{"mb-inbox", "label-we-have-never-seen"},
	}, nil)

	got, err := s.GetMessage(ctx, "work", "r-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.MailboxRemotes) != 1 || got.MailboxRemotes[0] != "mb-inbox" {
		t.Fatalf("MailboxRemotes = %v, want [mb-inbox]", got.MailboxRemotes)
	}
}

func TestHasMessageAndListRemoteIDs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	exists, complete, err := s.HasMessage(ctx, "work", "nope")
	if err != nil || exists || complete {
		t.Fatalf("HasMessage(missing) = %v %v %v", exists, complete, err)
	}

	putMessage(t, s, &model.Message{AccountID: "work", RemoteID: "full", ThreadID: "t1",
		BlobSHA256: "sha1", RawComplete: true, Received: base}, nil)
	putMessage(t, s, &model.Message{AccountID: "work", RemoteID: "partial", ThreadID: "t2",
		RawComplete: false, Received: base}, nil)

	if exists, complete, _ := s.HasMessage(ctx, "work", "full"); !exists || !complete {
		t.Errorf("HasMessage(full) = %v %v", exists, complete)
	}
	if exists, complete, _ := s.HasMessage(ctx, "work", "partial"); !exists || complete {
		t.Errorf("HasMessage(partial) = %v %v", exists, complete)
	}

	if _, err := s.MarkDeleted(ctx, "work", []string{"partial"}); err != nil {
		t.Fatal(err)
	}
	live, err := s.ListRemoteIDs(ctx, "work", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0] != "full" {
		t.Fatalf("ListRemoteIDs(live) = %v", live)
	}
	all, err := s.ListRemoteIDs(ctx, "work", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ListRemoteIDs(all) = %v", all)
	}
}

func TestUpdateMessageState(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "r-1", ThreadID: "th", Received: base,
		Subject: "subject stays", Flags: model.Flags{Unread: true},
		MailboxRemotes: []string{"mb-inbox"},
	}, &mime.Parsed{TextBody: "searchable body text"})

	// Archive it: read + moved out of the inbox.
	if err := s.UpdateMessageState(ctx, "work", "r-1",
		model.Flags{Unread: false, Flagged: true}, []string{"mb-archive"}); err != nil {
		t.Fatalf("UpdateMessageState: %v", err)
	}
	got, err := s.GetMessage(ctx, "work", "r-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Flags.Unread || !got.Flags.Flagged {
		t.Errorf("Flags = %+v", got.Flags)
	}
	if len(got.MailboxRemotes) != 1 || got.MailboxRemotes[0] != "mb-archive" {
		t.Errorf("MailboxRemotes = %v", got.MailboxRemotes)
	}
	if got.Subject != "subject stays" || got.TextBody != "searchable body text" {
		t.Errorf("body/subject clobbered by a state update: %+v", got)
	}

	// The thread summary follows the unread count.
	th, _, err := s.GetThread(ctx, "work", "th", false)
	if err != nil {
		t.Fatal(err)
	}
	if th.UnreadCount != 0 {
		t.Errorf("thread unread = %d, want 0", th.UnreadCount)
	}

	// A nil mailbox slice leaves membership alone.
	if err := s.UpdateMessageState(ctx, "work", "r-1", model.Flags{Unread: true}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMessage(ctx, "work", "r-1")
	if len(got.MailboxRemotes) != 1 || got.MailboxRemotes[0] != "mb-archive" {
		t.Errorf("nil mailboxes changed membership: %v", got.MailboxRemotes)
	}
	// An empty (non-nil) slice clears it.
	if err := s.UpdateMessageState(ctx, "work", "r-1", model.Flags{}, []string{}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMessage(ctx, "work", "r-1")
	if len(got.MailboxRemotes) != 0 {
		t.Errorf("empty slice did not clear membership: %v", got.MailboxRemotes)
	}

	if err := s.UpdateMessageState(ctx, "work", "unknown", model.Flags{}, nil); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("UpdateMessageState(missing) = %v, want ErrNotFound", err)
	}

	// The FTS index is untouched by flag updates.
	hits, err := s.Search(ctx, "searchable", MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("search after state updates found %d hits, want 1", len(hits))
	}
}

func TestMarkDeletedAndUndeleted(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	for i, remote := range []string{"a", "b", "c"} {
		putMessage(t, s, &model.Message{
			AccountID: "work", RemoteID: remote, ThreadID: "th",
			Received: base.Add(time.Duration(i) * time.Minute),
			Flags:    model.Flags{Unread: true}, MailboxRemotes: []string{"mb-inbox"},
		}, &mime.Parsed{TextBody: "message " + remote})
	}

	n, err := s.MarkDeleted(ctx, "work", []string{"a", "b", "never-existed"})
	if err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	if n != 2 {
		t.Fatalf("MarkDeleted reported %d, want 2", n)
	}
	// Marking again is a no-op.
	if n, err := s.MarkDeleted(ctx, "work", []string{"a"}); err != nil || n != 0 {
		t.Fatalf("second MarkDeleted = %d %v", n, err)
	}

	live, err := s.ListMessages(ctx, MessageFilter{Accounts: []string{"work"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].RemoteID != "c" {
		t.Fatalf("live messages = %d %v", len(live), live)
	}
	withDeleted, err := s.ListMessages(ctx, MessageFilter{IncludeDeleted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withDeleted) != 3 {
		t.Fatalf("IncludeDeleted list = %d, want 3", len(withDeleted))
	}

	del, err := s.GetMessage(ctx, "work", "a")
	if err != nil {
		t.Fatalf("deleted message must still be readable: %v", err)
	}
	if del.DeletedAt == nil {
		t.Error("DeletedAt not set")
	}
	if len(del.MailboxRemotes) != 0 {
		t.Errorf("deleted message still in mailboxes: %v", del.MailboxRemotes)
	}

	th, msgs, err := s.GetThread(ctx, "work", "th", false)
	if err != nil {
		t.Fatal(err)
	}
	if th.MessageCount != 1 || th.UnreadCount != 1 || len(msgs) != 1 {
		t.Errorf("thread after deletes: count=%d unread=%d msgs=%d", th.MessageCount, th.UnreadCount, len(msgs))
	}

	// Deleted messages stay out of search results.
	hits, err := s.Search(ctx, "message", MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("search returned %d deleted-inclusive hits, want 1", len(hits))
	}

	n, err = s.MarkUndeleted(ctx, "work", []string{"a"})
	if err != nil || n != 1 {
		t.Fatalf("MarkUndeleted = %d %v", n, err)
	}
	back, err := s.GetMessage(ctx, "work", "a")
	if err != nil {
		t.Fatal(err)
	}
	if back.DeletedAt != nil {
		t.Error("DeletedAt not cleared")
	}
	if hits, _ := s.Search(ctx, "message", MessageFilter{}); len(hits) != 2 {
		t.Errorf("undeleted message not searchable again: %d hits", len(hits))
	}
	if report, err := s.IntegrityCheck(ctx); err != nil || report != "ok" {
		t.Fatalf("IntegrityCheck after delete cycle = %q %v", report, err)
	}

	// Deleting every message in a thread removes the summary row.
	if _, err := s.MarkDeleted(ctx, "work", []string{"a", "b", "c"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetThread(ctx, "work", "th", false); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("empty thread summary survived: %v", err)
	}
}

// ---------------------------------------------------------------------------

func TestTxAtomicity(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	if err := s.SetState(ctx, "work", "mail", "state-0"); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("provider exploded mid-batch")
	err := s.Tx(ctx, func(tx *Tx) error {
		for i := range 3 {
			if _, err := tx.UpsertMessage(ctx, &model.Message{
				AccountID: "work", RemoteID: fmt.Sprintf("tx-%d", i), ThreadID: "tx",
				Received: base, MailboxRemotes: []string{"mb-inbox"},
			}, &mime.Parsed{TextBody: "transactional body"}); err != nil {
				return err
			}
		}
		if err := tx.SetState(ctx, "work", "mail", "state-1"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx error = %v, want the callback's error", err)
	}

	msgs, err := s.ListMessages(ctx, MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("rolled-back transaction left %d messages", len(msgs))
	}
	state, err := s.GetState(ctx, "work", "mail")
	if err != nil {
		t.Fatal(err)
	}
	if state != "state-0" {
		t.Fatalf("state advanced despite rollback: %q", state)
	}
	if hits, _ := s.Search(ctx, "transactional", MessageFilter{}); len(hits) != 0 {
		t.Fatalf("rolled-back transaction left %d FTS entries", len(hits))
	}
	var threads int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM threads`).Scan(&threads); err != nil {
		t.Fatal(err)
	}
	if threads != 0 {
		t.Fatalf("rolled-back transaction left %d thread rows", threads)
	}

	// The same batch, committed, lands everything at once.
	if err := s.Tx(ctx, func(tx *Tx) error {
		for i := range 3 {
			if _, err := tx.UpsertMessage(ctx, &model.Message{
				AccountID: "work", RemoteID: fmt.Sprintf("tx-%d", i), ThreadID: "tx",
				Received: base, MailboxRemotes: []string{"mb-inbox"},
			}, &mime.Parsed{TextBody: "transactional body"}); err != nil {
				return err
			}
		}
		return tx.SetState(ctx, "work", "mail", "state-1")
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	msgs, _ = s.ListMessages(ctx, MessageFilter{})
	if len(msgs) != 3 {
		t.Fatalf("committed transaction stored %d messages, want 3", len(msgs))
	}
	if state, _ := s.GetState(ctx, "work", "mail"); state != "state-1" {
		t.Fatalf("state = %q, want state-1", state)
	}
	th, _, err := s.GetThread(ctx, "work", "tx", false)
	if err != nil {
		t.Fatal(err)
	}
	if th.MessageCount != 3 {
		t.Fatalf("thread count = %d, want 3", th.MessageCount)
	}
}

func TestSyncStateAndBackfill(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	if st, err := s.GetState(ctx, "work", "mail"); err != nil || st != "" {
		t.Fatalf("unset state = %q %v, want empty", st, err)
	}
	if err := s.SetState(ctx, "work", "mail", "h-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetState(ctx, "work", "cal:primary", "tok-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetState(ctx, "work", "mail", "h-2"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.GetState(ctx, "work", "mail"); st != "h-2" {
		t.Fatalf("state = %q", st)
	}
	states, err := s.ListStates(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 {
		t.Fatalf("ListStates = %+v", states)
	}
	if states[0].UpdatedAt.IsZero() {
		t.Error("UpdatedAt not set")
	}
	if err := s.ClearState(ctx, "work", "mail"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.GetState(ctx, "work", "mail"); st != "" {
		t.Fatalf("state after clear = %q", st)
	}

	if _, err := s.GetBackfill(ctx, "work", "mail"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetBackfill(missing) = %v, want ErrNotFound", err)
	}
	bf := &Backfill{AccountID: "work", Resource: "mail", StateAtStart: "h-0", TotalHint: 1000}
	if err := s.SetBackfill(ctx, bf); err != nil {
		t.Fatal(err)
	}
	bf.Cursor = "page-2"
	bf.Done = 500
	if err := s.SetBackfill(ctx, bf); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBackfill(ctx, "work", "mail")
	if err != nil {
		t.Fatal(err)
	}
	if got.Cursor != "page-2" || got.Done != 500 || got.TotalHint != 1000 || got.StateAtStart != "h-0" {
		t.Fatalf("backfill = %+v", got)
	}
	if got.Finished() {
		t.Error("Finished() true before finishing")
	}
	done := base
	got.FinishedAt = &done
	if err := s.SetBackfill(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetBackfill(ctx, "work", "mail")
	if !got.Finished() || !got.FinishedAt.Equal(base) {
		t.Fatalf("FinishedAt = %v", got.FinishedAt)
	}

	// Mail and calendar backfills coexist for one account.
	if err := s.SetBackfill(ctx, &Backfill{AccountID: "work", Resource: "cal:primary", StateAtStart: "t0"}); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListBackfills(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ListBackfills = %+v", all)
	}
	if err := s.ClearBackfill(ctx, "work", "mail"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBackfill(ctx, "work", "mail"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("after ClearBackfill: %v", err)
	}
}

func TestOutboxLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	id1, err := s.EnqueueOutbox(ctx, "work", "send", []byte(`{"to":"a@b.c"}`))
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.EnqueueOutbox(ctx, "work", "flags", []byte(`{"unread":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatal("outbox ids not unique")
	}

	pending, err := s.ListOutbox(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("%d pending, want 2", len(pending))
	}
	if pending[0].Kind != "send" || string(pending[0].Payload) != `{"to":"a@b.c"}` {
		t.Fatalf("item = %+v", pending[0])
	}
	if pending[0].Attempts != 0 || pending[0].DoneAt != nil {
		t.Fatalf("fresh item = %+v", pending[0])
	}

	if err := s.MarkOutboxFailed(ctx, id1, "connection refused"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkOutboxFailed(ctx, id1, "connection refused again"); err != nil {
		t.Fatal(err)
	}
	item, err := s.GetOutbox(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	if item.Attempts != 2 || item.LastError != "connection refused again" {
		t.Fatalf("after failures = %+v", item)
	}

	if err := s.MarkOutboxDone(ctx, id1); err != nil {
		t.Fatal(err)
	}
	item, _ = s.GetOutbox(ctx, id1)
	if item.DoneAt == nil || item.LastError != "" {
		t.Fatalf("after done = %+v", item)
	}
	if pending, _ := s.ListOutbox(ctx, true); len(pending) != 1 || pending[0].ID != id2 {
		t.Fatalf("pending after done = %+v", pending)
	}
	if all, _ := s.ListOutbox(ctx, false); len(all) != 2 {
		t.Fatal("completed item disappeared from the full listing")
	}

	if err := s.DropOutbox(ctx, id2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOutbox(ctx, id2); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetOutbox(dropped) = %v", err)
	}
	if err := s.DropOutbox(ctx, 4242); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("DropOutbox(missing) = %v, want ErrNotFound", err)
	}
	if err := s.MarkOutboxDone(ctx, 4242); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("MarkOutboxDone(missing) = %v, want ErrNotFound", err)
	}

	n, err := s.PruneOutbox(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("PruneOutbox removed %d, want 1", n)
	}
}

func TestSyncLog(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	for i := range 5 {
		if _, err := s.AppendSyncLog(ctx, SyncLogEntry{
			AccountID: "work", Kind: "delta",
			Started:  base.Add(time.Duration(i) * time.Minute),
			Finished: base.Add(time.Duration(i)*time.Minute + 10*time.Second),
			Added:    i, Updated: 1, Removed: 0,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AppendSyncLog(ctx, SyncLogEntry{
		AccountID: "work", Kind: "reconcile", Started: base, Finished: base,
		Error: "state expired",
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := s.RecentSyncLog(ctx, "work", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("%d entries, want 3", len(entries))
	}
	if entries[0].Kind != "reconcile" || entries[0].Error != "state expired" {
		t.Fatalf("newest entry = %+v", entries[0])
	}
	if entries[1].Added != 4 {
		t.Fatalf("ordering wrong: %+v", entries[1])
	}
	if entries[1].Started.IsZero() || entries[1].Finished.IsZero() {
		t.Fatalf("timestamps lost: %+v", entries[1])
	}

	if all, err := s.RecentSyncLog(ctx, "", 100); err != nil || len(all) != 6 {
		t.Fatalf("RecentSyncLog(all) = %d %v", len(all), err)
	}
	if err := s.PruneSyncLog(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if kept, _ := s.RecentSyncLog(ctx, "work", 100); len(kept) != 2 {
		t.Fatalf("after prune: %d entries, want 2", len(kept))
	}
}

func TestAccountStats(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	putMessage(t, s, &model.Message{AccountID: "work", RemoteID: "a", ThreadID: "t1",
		BlobSHA256: "sha-a", RawComplete: true, Received: base,
		Flags: model.Flags{Unread: true}, MailboxRemotes: []string{"mb-inbox"}},
		&mime.Parsed{TextBody: "a", Attachments: []mime.Part{{Path: "2", Filename: "x.pdf"}}})
	putMessage(t, s, &model.Message{AccountID: "work", RemoteID: "b", ThreadID: "t1",
		BlobSHA256: "sha-b", RawComplete: true, Received: base.Add(time.Hour),
		Flags: model.Flags{Flagged: true}}, nil)
	putMessage(t, s, &model.Message{AccountID: "work", RemoteID: "c", ThreadID: "t2",
		RawComplete: false, Received: base.Add(2 * time.Hour)}, nil)
	if _, err := s.MarkDeleted(ctx, "work", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueOutbox(ctx, "work", "send", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}

	st, err := s.AccountStats(ctx, "work")
	if err != nil {
		t.Fatalf("AccountStats: %v", err)
	}
	if st.Messages != 3 || st.Unread != 1 || st.Flagged != 1 || st.Deleted != 1 {
		t.Errorf("counts = %+v", st)
	}
	if st.BlobsIncomplete != 1 {
		t.Errorf("BlobsIncomplete = %d, want 1", st.BlobsIncomplete)
	}
	if st.Threads != 1 {
		t.Errorf("Threads = %d, want 1 (t2 emptied by the delete)", st.Threads)
	}
	if st.Mailboxes != 5 {
		t.Errorf("Mailboxes = %d, want 5", st.Mailboxes)
	}
	if st.Attachments != 1 {
		t.Errorf("Attachments = %d, want 1", st.Attachments)
	}
	if st.OutboxPending != 1 {
		t.Errorf("OutboxPending = %d, want 1", st.OutboxPending)
	}
	if !st.LastReceived.Equal(base.Add(time.Hour)) {
		t.Errorf("LastReceived = %v, want the newest live message", st.LastReceived)
	}
	if st.LastIndexed.IsZero() {
		t.Error("LastIndexed not set")
	}
}

func TestMaintenance(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	putMessage(t, s, &model.Message{AccountID: "work", RemoteID: "a", ThreadID: "t",
		BlobSHA256: "sha-live", RawComplete: true, Received: base}, &mime.Parsed{TextBody: "one"})
	putMessage(t, s, &model.Message{AccountID: "work", RemoteID: "b", ThreadID: "t",
		BlobSHA256: "sha-dead", RawComplete: true, Received: base}, &mime.Parsed{TextBody: "two"})
	putMessage(t, s, &model.Message{AccountID: "work", RemoteID: "c", ThreadID: "t",
		BlobSHA256: "sha-live", RawComplete: true, Received: base}, &mime.Parsed{TextBody: "dup blob"})
	if _, err := s.MarkDeleted(ctx, "work", []string{"b"}); err != nil {
		t.Fatal(err)
	}

	seen := map[string]int{}
	if err := s.ReferencedBlobs(ctx, func(sha string) error { seen[sha]++; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen["sha-live"] != 1 || seen["sha-dead"] != 1 {
		t.Fatalf("ReferencedBlobs = %v (want each sha once)", seen)
	}

	live := map[string]bool{}
	if err := s.LiveBlobs(ctx, func(sha string) error { live[sha] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || !live["sha-live"] {
		t.Fatalf("LiveBlobs = %v", live)
	}

	stop := errors.New("stop")
	if err := s.ReferencedBlobs(ctx, func(string) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("ReferencedBlobs error = %v", err)
	}

	if err := s.RebuildFTS(ctx); err != nil {
		t.Fatalf("RebuildFTS: %v", err)
	}
	if hits, err := s.Search(ctx, "dup", MessageFilter{}); err != nil || len(hits) != 1 {
		t.Fatalf("search after rebuild = %d %v", len(hits), err)
	}
	if err := s.Optimize(ctx); err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if err := s.Vacuum(ctx); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	if report, err := s.IntegrityCheck(ctx); err != nil || report != "ok" {
		t.Fatalf("IntegrityCheck = %q %v", report, err)
	}
}

func TestConcurrentReadersAndWriter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	// The sync engine writes in batches while CLI reads run against WAL.
	writeDone := make(chan error, 1)
	go func() {
		for batch := range 10 {
			err := s.Tx(ctx, func(tx *Tx) error {
				for i := range 10 {
					if _, err := tx.UpsertMessage(ctx, &model.Message{
						AccountID: "work", RemoteID: fmt.Sprintf("c-%d-%d", batch, i),
						ThreadID: fmt.Sprintf("th-%d", batch), Received: base.Add(time.Duration(i) * time.Second),
						MailboxRemotes: []string{"mb-inbox"},
					}, &mime.Parsed{Subject: "concurrent", TextBody: "body text"}); err != nil {
						return err
					}
				}
				return tx.SetState(ctx, "work", "mail", fmt.Sprintf("state-%d", batch))
			})
			if err != nil {
				writeDone <- err
				return
			}
		}
		writeDone <- nil
	}()

	readErr := make(chan error, 4)
	for range 4 {
		go func() {
			for range 25 {
				if _, err := s.ListMessages(ctx, MessageFilter{Limit: 20}); err != nil {
					readErr <- err
					return
				}
				if _, err := s.Search(ctx, "concurrent", MessageFilter{Limit: 5}); err != nil {
					readErr <- err
					return
				}
			}
			readErr <- nil
		}()
	}

	if err := <-writeDone; err != nil {
		t.Fatalf("writer: %v", err)
	}
	for range 4 {
		if err := <-readErr; err != nil {
			t.Fatalf("reader: %v", err)
		}
	}

	n, err := s.CountMessages(ctx, MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Fatalf("%d messages after concurrent load, want 100", n)
	}
	if state, _ := s.GetState(ctx, "work", "mail"); state != "state-9" {
		t.Fatalf("state = %q", state)
	}
	if report, err := s.IntegrityCheck(ctx); err != nil || report != "ok" {
		t.Fatalf("IntegrityCheck = %q %v", report, err)
	}
}
