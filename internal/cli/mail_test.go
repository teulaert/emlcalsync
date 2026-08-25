package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lennert/emlcal/internal/mime"
	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/provider/fake"
)

// ---------------------------------------------------------------------------
// Fixtures

const mailQuotedBody = `Thanks, that works for me.

On Mon, 24 Aug 2026 at 09:00, Bob <bob@example.com> wrote:
> are we still on for Tuesday?
> — Bob
`

// mailAttachmentData is the payload of the seeded attachment.
var mailAttachmentData = []byte("id,amount\n1,42\n")

// mailRawWithAttachment builds a message carrying one attachment.
func mailRawWithAttachment(t *testing.T, from, to, subject, body string, date time.Time) []byte {
	t.Helper()
	raw, err := mime.Build(&mime.Draft{
		From:      model.Address{Name: "Alice", Email: from},
		To:        []model.Address{{Email: to}},
		Subject:   subject,
		TextBody:  body,
		Date:      date,
		MessageID: "att-hello@example.test",
		Attachments: []mime.DraftAttachment{{
			Filename:    "report.csv",
			ContentType: "text/csv",
			Data:        mailAttachmentData,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// mailSeed builds the standard fixture: three messages in "work" (one unread
// with an attachment, one filed in the WORK mailbox, one five days old) and
// one in "home". The two newest work messages share a thread.
func mailSeed(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)

	unread := fake.NewMsg("m-unread",
		mailRawWithAttachment(t, "alice@example.com", "me@example.com", "Hello there", "hello, the numbers are attached", env.Now.Add(-time.Hour))).
		WithFlags(model.Flags{Unread: true}).
		WithReceived(env.Now.Add(-time.Hour)).
		WithThread("t-1")

	filed := fake.NewMsg("m-work",
		RawMail(t, "bob@example.com", "me@example.com", "Re: Hello there", mailQuotedBody, env.Now.Add(-2*time.Hour))).
		WithMailboxes("WORK").
		WithReceived(env.Now.Add(-2 * time.Hour)).
		WithThread("t-1")

	old := fake.NewMsg("m-old",
		RawMail(t, "carol@example.com", "me@example.com", "Old news", "this happened last week", env.Now.Add(-5*24*time.Hour))).
		WithReceived(env.Now.Add(-5 * 24 * time.Hour)).
		WithThread("t-old")

	env.Seed("work", unread, filed, old)
	env.Seed("home", fake.NewMsg("h-1",
		RawMail(t, "dave@example.com", "me@gmail.example", "Personal", "dinner on friday?", env.Now.Add(-30*time.Minute))).
		WithReceived(env.Now.Add(-30*time.Minute)))
	return env
}

// mailRow mirrors the JSON of one `mail list` / `mail search` row.
type mailRow struct {
	ID             string   `json:"id"`
	ThreadID       string   `json:"thread_id"`
	Date           string   `json:"date"`
	Subject        string   `json:"subject"`
	Snippet        string   `json:"snippet"`
	Unread         bool     `json:"unread"`
	Flagged        bool     `json:"flagged"`
	Answered       bool     `json:"answered"`
	HasAttachments bool     `json:"has_attachments"`
	Mailboxes      []string `json:"mailboxes"`
	Account        string   `json:"account"`
	Rank           float64  `json:"rank"`
	From           struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"from"`
}

func mailList(t *testing.T, env *testEnv, args ...string) []mailRow {
	t.Helper()
	out := env.MustRun(append([]string{"mail"}, args...)...)
	var rows []mailRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode %v: %v\noutput: %s", args, err, out)
	}
	return rows
}

func mailIDs(rows []mailRow) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

func mailEqual(t *testing.T, got, want []string, what string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// ---------------------------------------------------------------------------
// Read commands

func TestMailList(t *testing.T) {
	env := mailSeed(t)

	rows := mailList(t, env, "list")
	mailEqual(t, mailIDs(rows),
		[]string{"home:h-1", "work:m-unread", "work:m-work", "work:m-old"}, "mail list")

	if rows[1].Mailboxes == nil || rows[1].Mailboxes[0] != "Inbox" {
		t.Errorf("work:m-unread mailboxes = %v, want [Inbox]", rows[1].Mailboxes)
	}
	if got := rows[2].Mailboxes; len(got) != 1 || got[0] != "Work" {
		t.Errorf("work:m-work mailboxes = %v, want [Work]", got)
	}
	if !rows[1].Unread || !rows[1].HasAttachments {
		t.Errorf("work:m-unread: unread=%v attachments=%v, want both true", rows[1].Unread, rows[1].HasAttachments)
	}
	if rows[1].ThreadID != "work:t:t-1" {
		t.Errorf("thread_id = %q, want work:t:t-1", rows[1].ThreadID)
	}
	if rows[1].From.Email != "alice@example.com" {
		t.Errorf("from = %+v", rows[1].From)
	}
}

func TestMailListFilters(t *testing.T) {
	env := mailSeed(t)

	rows := mailList(t, env, "list", "--account", "work", "--unread")
	mailEqual(t, mailIDs(rows), []string{"work:m-unread"}, "--account work --unread")

	rows = mailList(t, env, "list", "--since", "2d")
	mailEqual(t, mailIDs(rows),
		[]string{"home:h-1", "work:m-unread", "work:m-work"}, "--since 2d")

	rows = mailList(t, env, "list", "--mailbox", "work")
	mailEqual(t, mailIDs(rows), []string{"work:m-work"}, "--mailbox work")

	rows = mailList(t, env, "list", "--mailbox", "inbox", "--account", "work")
	mailEqual(t, mailIDs(rows), []string{"work:m-unread", "work:m-old"}, "--mailbox inbox")

	rows = mailList(t, env, "list", "--from", "carol")
	mailEqual(t, mailIDs(rows), []string{"work:m-old"}, "--from carol")

	rows = mailList(t, env, "list", "--limit", "1")
	mailEqual(t, mailIDs(rows), []string{"home:h-1"}, "--limit 1")

	rows = mailList(t, env, "list", "--limit", "1", "--offset", "1")
	mailEqual(t, mailIDs(rows), []string{"work:m-unread"}, "--offset 1")

	if _, _, code := env.Run("mail", "list", "--mailbox", "nope"); code != 3 {
		t.Errorf("--mailbox nope exit = %d, want 3", code)
	}
}

func TestMailListThreads(t *testing.T) {
	env := mailSeed(t)
	out := env.MustRun("mail", "list", "--thread", "--account", "work")
	var rows []struct {
		ID      string `json:"id"`
		Subject string `json:"subject"`
		Count   int    `json:"count"`
		Unread  int    `json:"unread"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("threads = %d, want 2\n%s", len(rows), out)
	}
	if rows[0].ID != "work:t:t-1" || rows[0].Count != 2 || rows[0].Unread != 1 {
		t.Errorf("first thread = %+v", rows[0])
	}
}

func TestMailMailboxes(t *testing.T) {
	env := mailSeed(t)
	out := env.MustRun("mail", "mailboxes", "--account", "work")
	var rows []struct {
		Account string `json:"account"`
		Name    string `json:"name"`
		Role    string `json:"role"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	var names []string
	for _, r := range rows {
		if r.Account != "work" {
			t.Errorf("account = %q", r.Account)
		}
		names = append(names, r.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "Inbox") || !strings.Contains(strings.Join(names, ","), "Work") {
		t.Errorf("mailbox names = %v, want Inbox and Work", names)
	}
}

func TestMailSearch(t *testing.T) {
	env := mailSeed(t)

	rows := mailList(t, env, "search", "hello")
	if len(rows) == 0 {
		t.Fatal("search hello found nothing")
	}
	found := false
	for _, r := range rows {
		if r.ID == "work:m-unread" {
			found = true
		}
	}
	if !found {
		t.Errorf("search hello = %v, want work:m-unread", mailIDs(rows))
	}

	rows = mailList(t, env, "search", "dinner")
	mailEqual(t, mailIDs(rows), []string{"home:h-1"}, "search dinner")

	_, errs, code := env.Run("mail", "search", `"unterminated`)
	if code != 2 {
		t.Fatalf("bad query exit = %d, want 2 (stderr: %s)", code, errs)
	}
	if !strings.Contains(errs, "AND/OR/NOT") {
		t.Errorf("bad query error lacks a hint: %s", errs)
	}
}

func TestMailRead(t *testing.T) {
	env := mailSeed(t)

	var msg struct {
		ID          string   `json:"id"`
		Subject     string   `json:"subject"`
		Body        string   `json:"body"`
		Mailboxes   []string `json:"mailboxes"`
		MessageID   string   `json:"message_id"`
		Attachments []struct {
			Part     string `json:"part"`
			Filename string `json:"filename"`
			Size     int64  `json:"size"`
		} `json:"attachments"`
	}
	out := env.MustRun("mail", "read", "work:m-work")
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if !strings.Contains(msg.Body, "Thanks, that works for me.") {
		t.Errorf("body lost the actual text: %q", msg.Body)
	}
	if strings.Contains(msg.Body, "> are we still on for Tuesday?") {
		t.Errorf("body kept the quoted block: %q", msg.Body)
	}

	out = env.MustRun("mail", "read", "work:m-work", "--full")
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		t.Fatalf("decode --full: %v\n%s", err, out)
	}
	if !strings.Contains(msg.Body, "> are we still on for Tuesday?") {
		t.Errorf("--full dropped the quoted block: %q", msg.Body)
	}

	raw := env.MustRun("mail", "read", "work:m-work", "--raw")
	if !strings.Contains(raw, "Subject: Re: Hello there") || !strings.Contains(raw, "From: <bob@example.com>") {
		t.Errorf("--raw is not an RFC 822 message:\n%s", raw)
	}
	if !strings.HasPrefix(raw, "Mime-Version:") && !strings.HasPrefix(raw, "MIME-Version:") &&
		!strings.HasPrefix(raw, "From:") && !strings.HasPrefix(raw, "Date:") &&
		!strings.HasPrefix(raw, "Content-Type:") && !strings.HasPrefix(raw, "Message-Id:") &&
		!strings.HasPrefix(raw, "Message-ID:") && !strings.HasPrefix(raw, "Subject:") {
		t.Errorf("--raw does not start with a header line: %.60q", raw)
	}

	out = env.MustRun("mail", "read", "work:m-unread")
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		t.Fatalf("decode attachment message: %v\n%s", err, out)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].Filename != "report.csv" {
		t.Fatalf("attachments = %+v", msg.Attachments)
	}

	out = env.MustRun("mail", "read", "work:m-work", "--headers")
	if !strings.Contains(out, "\"headers\"") || !strings.Contains(out, "Message-Id") && !strings.Contains(out, "Message-ID") {
		t.Errorf("--headers output: %s", out)
	}

	if _, _, code := env.Run("mail", "read", "work:nope"); code != 3 {
		t.Errorf("unknown id exit = %d, want 3", code)
	}
	if _, _, code := env.Run("mail", "read", "not-an-id"); code != 2 {
		t.Errorf("bad id exit = %d, want 2", code)
	}
}

func TestMailReadPlain(t *testing.T) {
	env := mailSeed(t)
	out := env.MustRun("mail", "read", "work:m-work", "-o", "table")
	head, body, ok := strings.Cut(out, "\n\n")
	if !ok {
		t.Fatalf("no header block:\n%s", out)
	}
	if !strings.Contains(head, "From:") || !strings.Contains(head, "Subject: Re: Hello there") {
		t.Errorf("header block = %q", head)
	}
	if !strings.Contains(body, "Thanks, that works for me.") {
		t.Errorf("body = %q", body)
	}
}

func TestMailThread(t *testing.T) {
	env := mailSeed(t)
	var th struct {
		ID       string `json:"id"`
		Subject  string `json:"subject"`
		Count    int    `json:"count"`
		Messages []struct {
			ID   string `json:"id"`
			Body string `json:"body"`
		} `json:"messages"`
	}
	for _, id := range []string{"work:t:t-1", "work:m-unread"} {
		out := env.MustRun("mail", "thread", id)
		if err := json.Unmarshal([]byte(out), &th); err != nil {
			t.Fatalf("decode %s: %v\n%s", id, err, out)
		}
		if th.Count != 2 {
			t.Fatalf("%s: count = %d, want 2", id, th.Count)
		}
		if th.Messages[0].ID != "work:m-work" || th.Messages[1].ID != "work:m-unread" {
			t.Errorf("%s: messages = %s, %s; want oldest first", id, th.Messages[0].ID, th.Messages[1].ID)
		}
		if strings.Contains(th.Messages[0].Body, "> are we still on") {
			t.Errorf("thread bodies are not stripped: %q", th.Messages[0].Body)
		}
	}
}

func TestMailAttachment(t *testing.T) {
	env := mailSeed(t)

	out := env.MustRun("mail", "attachment", "list", "work:m-unread")
	var atts []struct {
		Part        string `json:"part"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
	}
	if err := json.Unmarshal([]byte(out), &atts); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(atts) != 1 || atts[0].Filename != "report.csv" {
		t.Fatalf("attachments = %+v", atts)
	}

	dest := filepath.Join(t.TempDir(), "out.csv")
	got := env.MustRun("mail", "attachment", "get", "work:m-unread", "report.csv", "-O", dest)
	if !strings.Contains(got, dest) {
		t.Errorf("get output = %s, want the path %s", got, dest)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(mailAttachmentData) {
		t.Errorf("attachment bytes = %q, want %q", data, mailAttachmentData)
	}

	dest2 := filepath.Join(t.TempDir(), "by-part.csv")
	env.MustRun("mail", "attachment", "get", "work:m-unread", atts[0].Part, "-O", dest2)
	if data, err := os.ReadFile(dest2); err != nil || string(data) != string(mailAttachmentData) {
		t.Errorf("by part path: %v / %q", err, data)
	}

	if _, _, code := env.Run("mail", "attachment", "get", "work:m-unread", "nope.pdf", "-O", dest); code != 3 {
		t.Errorf("unknown attachment exit = %d, want 3", code)
	}
}

// ---------------------------------------------------------------------------
// Write commands

func TestMailMark(t *testing.T) {
	env := mailSeed(t)

	env.MustRun("mail", "mark", "work:m-unread", "--read")

	flags, _, ok := env.Mail["work"].Lookup("m-unread")
	if !ok || flags.Unread {
		t.Errorf("provider flags = %+v (ok=%v), want unread=false", flags, ok)
	}
	rows := mailList(t, env, "list", "--account", "work", "--unread")
	if len(rows) != 0 {
		t.Errorf("still unread locally: %v", mailIDs(rows))
	}

	if _, _, code := env.Run("mail", "mark", "work:m-old"); code != 2 {
		t.Errorf("mark without a flag exit = %d, want 2", code)
	}
}

func TestMailMarkQueuesWhenOffline(t *testing.T) {
	env := mailSeed(t)
	env.Mail["work"].FailNext(1)

	_, _, code := env.Run("mail", "mark", "work:m-old", "--flag")
	if code != 6 {
		t.Fatalf("offline mark exit = %d, want 6", code)
	}
	rows := mailList(t, env, "list", "--account", "work", "--flagged")
	mailEqual(t, mailIDs(rows), []string{"work:m-old"}, "locally flagged while queued")
}

func TestMailMoveArchiveTrash(t *testing.T) {
	env := mailSeed(t)

	env.MustRun("mail", "move", "work:m-unread", "--to", "work")
	_, boxes, ok := env.Mail["work"].Lookup("m-unread")
	if !ok || !mailContains(boxes, "WORK") || mailContains(boxes, "INBOX") {
		t.Errorf("after move mailboxes = %v, want [WORK]", boxes)
	}

	env.MustRun("mail", "archive", "work:m-old")
	if _, boxes, _ := env.Mail["work"].Lookup("m-old"); mailContains(boxes, "INBOX") {
		t.Errorf("after archive mailboxes = %v, want no INBOX", boxes)
	}

	env.MustRun("mail", "trash", "work:m-work")
	if _, boxes, _ := env.Mail["work"].Lookup("m-work"); !mailContains(boxes, "TRASH") {
		t.Errorf("after trash mailboxes = %v, want TRASH", boxes)
	}

	if _, _, code := env.Run("mail", "move", "work:m-old", "--to", "nope"); code != 3 {
		t.Errorf("move to unknown mailbox exit = %d, want 3", code)
	}
}

func mailContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestMailReplyDryRun(t *testing.T) {
	env := mailSeed(t)
	out := env.MustRun("mail", "reply", "work:m-unread", "--body", "thanks", "--dry-run")

	for _, want := range []string{
		"In-Reply-To:",
		"Subject: Re: Hello there",
		"From: <me@example.com>",
		"To: ",
		"thanks",
		"> hello, the numbers are attached",
		"wrote:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "alice@example.com") {
		t.Errorf("dry-run does not address the sender:\n%s", out)
	}
	if n := len(env.Mail["work"].Sent()); n != 0 {
		t.Errorf("--dry-run sent %d messages", n)
	}
	if n := len(env.Mail["work"].Drafts()); n != 0 {
		t.Errorf("--dry-run stored %d drafts", n)
	}
}

func TestMailReplySends(t *testing.T) {
	env := mailSeed(t)
	out := env.MustRun("mail", "reply", "work:m-unread", "--body", "thanks")

	sent := env.Mail["work"].Sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %d messages, want 1\n%s", len(sent), out)
	}
	raw := string(sent[0])
	if !strings.Contains(raw, "Subject: Re: Hello there") || !strings.Contains(raw, "In-Reply-To:") {
		t.Errorf("sent message:\n%s", raw)
	}

	flags, _, ok := env.Mail["work"].Lookup("m-unread")
	if !ok || !flags.Answered {
		t.Errorf("original flags = %+v, want answered", flags)
	}
	rows := mailList(t, env, "list", "--account", "work")
	for _, r := range rows {
		if r.ID == "work:m-unread" && !r.Answered {
			t.Errorf("original not answered locally: %+v", r)
		}
	}
}

func TestMailReplyAll(t *testing.T) {
	env := newTestEnv(t)
	raw, err := mime.Build(&mime.Draft{
		From:      model.Address{Name: "Alice", Email: "alice@example.com"},
		To:        []model.Address{{Email: "me@example.com"}, {Email: "ted@example.com"}},
		Cc:        []model.Address{{Email: "zoe@example.com"}},
		Subject:   "Planning",
		TextBody:  "who is in?",
		Date:      env.Now.Add(-time.Hour),
		MessageID: "planning@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	env.Seed("work", fake.NewMsg("m-all", raw).WithReceived(env.Now.Add(-time.Hour)))

	out := env.MustRun("mail", "reply", "work:m-all", "--body", "me", "--all", "--dry-run")
	for _, want := range []string{"alice@example.com", "ted@example.com", "zoe@example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("--all output missing %s:\n%s", want, out)
		}
	}
	if strings.Count(out, "me@example.com") != 1 { // only the From header
		t.Errorf("--all replied to myself:\n%s", out)
	}
}

func TestMailSendAccountResolution(t *testing.T) {
	env := mailSeed(t)

	_, _, code := env.Run("mail", "send", "--to", "x@y.example", "--subject", "s", "--body", "b")
	if code != 2 {
		t.Fatalf("send without an account exit = %d, want 2", code)
	}
	if n := len(env.Mail["work"].Sent()); n != 0 {
		t.Fatalf("sent %d messages anyway", n)
	}

	env.MustRun("mail", "send", "--account", "work", "--to", "x@y.example", "--subject", "s", "--body", "b")
	sent := env.Mail["work"].Sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %d, want 1", len(sent))
	}
	raw := string(sent[0])
	if !strings.Contains(raw, "From: <me@example.com>") || !strings.Contains(raw, "To: <x@y.example>") {
		t.Errorf("sent message:\n%s", raw)
	}
}

func TestMailSendBodyFileAndAttachment(t *testing.T) {
	env := mailSeed(t)
	dir := t.TempDir()
	bodyPath := filepath.Join(dir, "body.txt")
	if err := os.WriteFile(bodyPath, []byte("from a file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	attPath := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(attPath, []byte("attached notes"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := env.MustRun("mail", "send", "--account", "work", "--to", "x@y.example,z@y.example",
		"--subject", "files", "--body-file", bodyPath, "--attach", attPath, "--dry-run")
	for _, want := range []string{"from a file", "notes.txt", "x@y.example", "z@y.example"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run missing %q:\n%s", want, out)
		}
	}

	env.Stdin = "piped body\n"
	out = env.MustRun("mail", "send", "--account", "work", "--to", "x@y.example",
		"--subject", "stdin", "--body-file", "-", "--dry-run")
	if !strings.Contains(out, "piped body") {
		t.Errorf("--body-file - did not read stdin:\n%s", out)
	}

	if _, _, code := env.Run("mail", "send", "--account", "work", "--to", "not an address",
		"--subject", "s", "--body", "b"); code != 2 {
		t.Errorf("bad address exit = %d, want 2", code)
	}
	if _, _, code := env.Run("mail", "send", "--account", "work", "--to", "x@y.example",
		"--subject", "s"); code != 2 {
		t.Errorf("missing body exit = %d, want 2", code)
	}
}

func TestMailDraft(t *testing.T) {
	env := mailSeed(t)
	env.MustRun("mail", "draft", "--account", "work", "--to", "x@y.example",
		"--subject", "later", "--body", "not yet")

	drafts := env.Mail["work"].Drafts()
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if !strings.Contains(string(drafts[0]), "Subject: later") {
		t.Errorf("draft:\n%s", drafts[0])
	}
	if n := len(env.Mail["work"].Sent()); n != 0 {
		t.Errorf("draft sent %d messages", n)
	}
}

// mailHTMLRaw is a hand-built multipart/alternative message, since mime.Build
// only produces text parts.
func mailHTMLRaw(date time.Time) []byte {
	return []byte(strings.ReplaceAll(`From: alice@example.com
To: me@example.com
Subject: Newsletter
Date: `+date.Format(time.RFC1123Z)+`
Message-ID: <html-1@example.test>
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="b1"

--b1
Content-Type: text/plain; charset=utf-8

plain version
--b1
Content-Type: text/html; charset=utf-8

<html><body><p>rich version</p></body></html>
--b1--
`, "\n", "\r\n"))
}

func TestMailReadHTML(t *testing.T) {
	env := newTestEnv(t)
	env.Seed("work", fake.NewMsg("m-html", mailHTMLRaw(env.Now.Add(-time.Hour))).
		WithReceived(env.Now.Add(-time.Hour)))

	var msg struct {
		Body string `json:"body"`
	}
	out := env.MustRun("mail", "read", "work:m-html", "--html")
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if !strings.Contains(msg.Body, "<p>rich version</p>") {
		t.Errorf("--html body = %q", msg.Body)
	}

	out = env.MustRun("mail", "read", "work:m-html")
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		t.Fatalf("decode default: %v\n%s", err, out)
	}
	if !strings.Contains(msg.Body, "plain version") {
		t.Errorf("default body = %q", msg.Body)
	}
}

func TestMailSendExistingDraft(t *testing.T) {
	env := mailSeed(t)
	out := env.MustRun("mail", "send", "--draft", "work:m-old")

	sent := env.Mail["work"].Sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %d, want 1", len(sent))
	}
	if !strings.Contains(string(sent[0]), "Subject: Old news") {
		t.Errorf("sent draft:\n%s", sent[0])
	}

	// The draft itself is a separate message on the server: sending its bytes
	// leaves it behind, so it goes to the trash.
	var row struct {
		Account      string `json:"account"`
		DraftTrashed *bool  `json:"draft_trashed"`
	}
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if row.DraftTrashed == nil || !*row.DraftTrashed {
		t.Errorf("draft_trashed = %v, want true (%s)", row.DraftTrashed, out)
	}
	if _, boxes, ok := env.Mail["work"].Lookup("m-old"); !ok || !mailContains(boxes, "TRASH") {
		t.Errorf("draft mailboxes = %v, want TRASH", boxes)
	}
	rows := mailList(t, env, "list", "--account", "work")
	for _, r := range rows {
		if r.ID == "work:m-old" && mailContains(r.Mailboxes, "Inbox") {
			t.Errorf("draft still in the inbox locally: %+v", r)
		}
	}
}

// A send that failed before any request bytes left the machine is safe to
// replay, so it is queued in the outbox (exit 6) and the draft stays put.
func TestMailSendDraftKeepsDraftWhenQueued(t *testing.T) {
	env := mailSeed(t)
	env.Mail["work"].FailNext(1) // pre-request failure: no connection at all

	_, errs, code := env.Run("mail", "send", "--draft", "work:m-old")
	if code != 6 {
		t.Fatalf("queued send --draft exit = %d, want 6\nstderr: %s", code, errs)
	}
	if n := len(env.Mail["work"].Sent()); n != 0 {
		t.Errorf("sent = %d, want 0 (the send is still queued)", n)
	}
	if _, boxes, _ := env.Mail["work"].Lookup("m-old"); mailContains(boxes, "TRASH") {
		t.Errorf("queued send trashed the draft before it went out: %v", boxes)
	}
}

// A send whose connection dropped mid-request may already have arrived, so the
// engine retires it instead of retrying: exit 4, nothing queued, draft intact.
func TestMailSendDraftKeepsDraftWhenOffline(t *testing.T) {
	env := mailSeed(t)
	env.Mail["work"].FailNextAmbiguous(1)

	_, errs, code := env.Run("mail", "send", "--draft", "work:m-old")
	if code != 4 {
		t.Fatalf("ambiguous send --draft exit = %d, want 4\nstderr: %s", code, errs)
	}
	if !strings.Contains(errs, "not sent") || !strings.Contains(errs, "not queued") {
		t.Errorf("offline error does not say the send did not happen: %s", errs)
	}
	if n := len(env.Mail["work"].Sent()); n != 0 {
		t.Errorf("sent = %d, want 0", n)
	}
	if _, boxes, _ := env.Mail["work"].Lookup("m-old"); mailContains(boxes, "TRASH") {
		t.Errorf("failed send trashed the draft anyway: %v", boxes)
	}
}

// Whether the reply is queued or retired, it has not been delivered, so the
// original must not be marked answered.
func TestMailReplyOfflineDoesNotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(t *testing.T, env *testEnv)
		exit int
	}{
		{"queued", func(_ *testing.T, env *testEnv) { env.Mail["work"].FailNext(1) }, 6},
		{"retired", func(_ *testing.T, env *testEnv) { env.Mail["work"].FailNextAmbiguous(1) }, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := mailSeed(t)
			tc.arm(t, env)

			_, errs, code := env.Run("mail", "reply", "work:m-unread", "--body", "thanks")
			if code != tc.exit {
				t.Fatalf("offline reply exit = %d, want %d\nstderr: %s", code, tc.exit, errs)
			}
			if flags, _, _ := env.Mail["work"].Lookup("m-unread"); flags.Answered {
				t.Errorf("original marked answered after an undelivered reply")
			}
			rows := mailList(t, env, "list", "--account", "work")
			for _, r := range rows {
				if r.ID == "work:m-unread" && r.Answered {
					t.Errorf("original answered locally after an undelivered reply: %+v", r)
				}
			}
		})
	}
}
