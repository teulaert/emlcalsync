package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lennert/emlcal/internal/testutil/jmapfake"
)

// ---------------------------------------------------------------------------
// (a) account add

func TestAccountAddFastmail(t *testing.T) {
	e := newEnv(t)

	stdout, stderr, code := e.runInput(jmapfake.DefaultToken+"\n",
		"account", "add", "fastmail", "--name", "work", "--email", "me@example.com", "--token-stdin")
	if code != 0 {
		t.Fatalf("account add: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	row := decodeObject(t, stdout)
	if got := str(t, row, "name"); got != "work" {
		t.Errorf("name = %q, want work", got)
	}
	if got := str(t, row, "provider"); got != "fastmail" {
		t.Errorf("provider = %q, want fastmail", got)
	}
	// The fake serves the six standard role mailboxes; `account add` verifies
	// the token by listing them.
	if got := num(t, row, "mailboxes"); got != 6 {
		t.Errorf("mailboxes = %v, want 6", got)
	}

	if _, err := os.Stat(e.configPath()); err != nil {
		t.Fatalf("config.toml not written: %v", err)
	}
	cfg, err := os.ReadFile(e.configPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^name\s+= "work"$`),
		regexp.MustCompile(`(?m)^provider\s+= "fastmail"$`),
		regexp.MustCompile(`(?m)^email\s+= "me@example.com"$`),
		regexp.MustCompile(`(?m)^\[\[accounts\]\]$`),
	} {
		if !want.Match(cfg) {
			t.Errorf("config.toml does not match %s:\n%s", want, cfg)
		}
	}

	secret := filepath.Join(e.secretsDir(), "work.fastmail.token")
	fi, err := os.Stat(secret)
	if err != nil {
		t.Fatalf("secret not written: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("secret mode = %o, want 600", perm)
	}
	if b, err := os.ReadFile(secret); err != nil || strings.TrimSpace(string(b)) != jmapfake.DefaultToken {
		t.Errorf("secret contents = %q (err %v), want the token", b, err)
	}
	if dir, err := os.Stat(e.secretsDir()); err == nil {
		if perm := dir.Mode().Perm(); perm != 0o700 {
			t.Errorf("secrets dir mode = %o, want 700", perm)
		}
	}
}

func TestAccountAddDoesNotPinConcurrencyToZero(t *testing.T) {

	e := newEnv(t)
	e.addAccount("work", "me@example.com")
	cfg, err := os.ReadFile(e.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?m)^concurrency\s*=\s*0$`).Match(cfg) {
		t.Errorf("config.toml pins concurrency to 0:\n%s", cfg)
	}
}

// ---------------------------------------------------------------------------
// (b) sync, status, blobs, second pass

func TestSyncBackfillThenDelta(t *testing.T) {
	e := newEnv(t)
	seedMessages(t, e.fake)
	e.addAccount("work", "me@example.com")

	out := e.mustRun("sync")
	rows := decodeArray(t, out)
	mailRow := findRow(t, rows, "resource", "mail")
	if got := num(t, mailRow, "added"); got != 5 {
		t.Errorf("first sync added = %v, want 5\n%s", got, out)
	}
	if got := str(t, mailRow, "kind"); got != "backfill" {
		t.Errorf("first sync kind = %q, want backfill", got)
	}

	statusOut := e.mustRun("status")
	status := decodeObject(t, statusOut)
	accounts, ok := status["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("status accounts = %v", status["accounts"])
	}
	acct := accounts[0].(map[string]any)
	if got := num(t, acct, "messages"); got != 5 {
		t.Errorf("status messages = %v, want 5", got)
	}
	if got := num(t, acct, "unread"); got != 1 {
		t.Errorf("status unread = %v, want 1", got)
	}

	if blobs := e.blobFiles(); len(blobs) != 5 {
		t.Errorf("blobs = %d (%v), want 5 *.eml.zst", len(blobs), blobs)
	}

	out2 := e.mustRun("sync")
	rows2 := decodeArray(t, out2)
	mail2 := findRow(t, rows2, "resource", "mail")
	if got := num(t, mail2, "added"); got != 0 {
		t.Errorf("second sync added = %v, want 0\n%s", got, out2)
	}
	if got := str(t, mail2, "kind"); got != "delta" {
		t.Errorf("second sync kind = %q, want delta", got)
	}
}

// TestSyncPagesThroughABigMailbox forces the enumeration to take more than one
// page: the fake advertises maxObjectsInGet=50, so 60 messages make the client
// use the anchor/anchorOffset cursor for the second page.
func TestSyncPagesThroughABigMailbox(t *testing.T) {
	e := newEnv(t)
	const n = 60
	for i := range n {
		raw := makeMessage(t, fmt.Sprintf("bulk-%d@example.com", i),
			fmt.Sprintf("Bulk message %02d", i), "sender@example.com",
			fmt.Sprintf("Body of message %d.\n", i))
		e.fake.AddMessage(raw, []string{jmapfake.MailboxInbox}, map[string]bool{"$seen": true})
	}
	e.addAccount("work", "me@example.com")

	out := e.mustRun("sync")
	mailRow := findRow(t, decodeArray(t, out), "resource", "mail")
	if got := num(t, mailRow, "added"); got != n {
		t.Fatalf("added = %v, want %d\n%s", got, n, out)
	}
	// More than one Email/query means the cursor really was used.
	if q := len(e.fake.CallsFor("Email/query")); q < 2 {
		t.Errorf("Email/query was called %d times; 60 messages at 50 per page should page", q)
	}
	anchored := 0
	for _, args := range e.fake.CallsFor("Email/query") {
		if _, ok := args["anchor"]; ok {
			anchored++
		}
	}
	if anchored == 0 {
		t.Errorf("no Email/query used an anchor; paging fell back to positions: %v",
			e.fake.CallsFor("Email/query"))
	}

	rows := decodeArray(t, e.mustRun("mail", "list", "--limit", "100"))
	if len(rows) != n {
		t.Errorf("mail list = %d rows, want %d", len(rows), n)
	}
	if got := str(t, rows[0], "subject"); got != "Bulk message 59" {
		t.Errorf("newest message = %q, want Bulk message 59", got)
	}
	if blobs := e.blobFiles(); len(blobs) != n {
		t.Errorf("blobs = %d, want %d", len(blobs), n)
	}
}

// ---------------------------------------------------------------------------
// (c) read commands

func TestMailListAndFilters(t *testing.T) {
	e, s := setup(t)

	rows := decodeArray(t, e.mustRun("mail", "list"))
	if len(rows) != 5 {
		t.Fatalf("mail list = %d rows, want 5:\n%v", len(rows), rows)
	}
	// Newest first: the fake stamps receivedAt in insertion order.
	want := []string{pub(s, "unread"), pub(s, "kickoff"), pub(s, "invoice"), pub(s, "reply"), pub(s, "weekly")}
	if got := ids(t, rows); !equalStrings(got, want) {
		t.Errorf("mail list ids = %v, want %v (newest first)", got, want)
	}

	unread := decodeArray(t, e.mustRun("mail", "list", "--unread"))
	if len(unread) != 1 || str(t, unread[0], "id") != pub(s, "unread") {
		t.Errorf("mail list --unread = %v, want just %s", ids(t, unread), pub(s, "unread"))
	}

	work := decodeArray(t, e.mustRun("mail", "list", "--mailbox", "work"))
	if len(work) != 1 || str(t, work[0], "id") != pub(s, "kickoff") {
		t.Errorf("mail list --mailbox work = %v, want just %s", ids(t, work), pub(s, "kickoff"))
	}

	inbox := decodeArray(t, e.mustRun("mail", "list", "--mailbox", "inbox"))
	if len(inbox) != 4 {
		t.Errorf("mail list --mailbox inbox = %d rows, want 4", len(inbox))
	}
}

func TestMailSearch(t *testing.T) {
	e, s := setup(t)

	hits := decodeArray(t, e.mustRun("mail", "search", "invoice"))
	if len(hits) != 1 {
		t.Fatalf("search invoice = %d hits, want 1: %v", len(hits), ids(t, hits))
	}
	if got := str(t, hits[0], "id"); got != pub(s, "invoice") {
		t.Errorf("search invoice hit = %s, want %s", got, pub(s, "invoice"))
	}
	if got := str(t, hits[0], "subject"); got != "Invoice attached" {
		t.Errorf("subject = %q", got)
	}

	kick := decodeArray(t, e.mustRun("mail", "search", "kickoff"))
	if len(kick) != 1 || str(t, kick[0], "id") != pub(s, "kickoff") {
		t.Errorf("search kickoff = %v, want %s", ids(t, kick), pub(s, "kickoff"))
	}

	none := decodeArray(t, e.mustRun("mail", "search", "zzzznotpresent"))
	if len(none) != 0 {
		t.Errorf("search for a missing word returned %v", ids(t, none))
	}
}

func TestMailReadStripsQuotes(t *testing.T) {
	e, s := setup(t)

	msg := decodeObject(t, e.mustRun("mail", "read", pub(s, "reply")))
	body := str(t, msg, "body")
	if !strings.Contains(body, "Sounds good to me.") {
		t.Errorf("body missing the reply text:\n%s", body)
	}
	if strings.Contains(body, "> The weekly report is ready.") {
		t.Errorf("quoted block was not stripped:\n%s", body)
	}
	if strings.Contains(body, "alice wrote:") {
		t.Errorf("attribution line was not stripped:\n%s", body)
	}
	if got := str(t, msg, "subject"); got != "Re: Weekly report" {
		t.Errorf("subject = %q", got)
	}
	from, ok := msg["from"].(map[string]any)
	if !ok || str(t, from, "email") != "bob@example.com" {
		t.Errorf("from = %v", msg["from"])
	}

	full := decodeObject(t, e.mustRun("mail", "read", pub(s, "reply"), "--full"))
	if !strings.Contains(str(t, full, "body"), "> The weekly report is ready.") {
		t.Errorf("--full dropped the quoted block:\n%s", str(t, full, "body"))
	}

	raw := e.mustRun("mail", "read", pub(s, "weekly"), "--raw")
	if !strings.HasPrefix(raw, "Date: ") {
		t.Errorf("--raw does not start with a header line: %.80q", raw)
	}
	if !strings.Contains(raw, "Subject: Weekly report") {
		t.Errorf("--raw missing the subject header:\n%.400s", raw)
	}
}

func TestMailThread(t *testing.T) {
	e, s := setup(t)

	out := decodeObject(t, e.mustRun("mail", "thread", pub(s, "weekly")))
	if got := num(t, out, "count"); got != 2 {
		t.Fatalf("thread count = %v, want 2\n%s", got, out)
	}
	msgs, ok := out["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("thread messages = %v", out["messages"])
	}
	first := msgs[0].(map[string]any)
	if got := str(t, first, "id"); got != pub(s, "weekly") {
		t.Errorf("thread is not oldest first: first = %s", got)
	}
	// The same thread is reachable from the reply's id, and from the thread id.
	viaReply := decodeObject(t, e.mustRun("mail", "thread", pub(s, "reply")))
	if str(t, viaReply, "id") != str(t, out, "id") {
		t.Errorf("thread id via reply = %s, via original = %s",
			str(t, viaReply, "id"), str(t, out, "id"))
	}
	threadID := str(t, out, "id")
	if !strings.HasPrefix(threadID, "work:t:") {
		t.Errorf("thread id = %q, want work:t:<id>", threadID)
	}
	viaThreadID := decodeObject(t, e.mustRun("mail", "thread", threadID))
	if got := num(t, viaThreadID, "count"); got != 2 {
		t.Errorf("thread by thread id: count = %v, want 2", got)
	}
}

func TestMailAttachmentRoundTrip(t *testing.T) {
	e, s := setup(t)

	rows := decodeArray(t, e.mustRun("mail", "attachment", "list", pub(s, "invoice")))
	if len(rows) != 1 {
		t.Fatalf("attachment list = %d rows, want 1: %v", len(rows), rows)
	}
	if got := str(t, rows[0], "filename"); got != "invoice.csv" {
		t.Errorf("filename = %q, want invoice.csv", got)
	}
	if got := str(t, rows[0], "content_type"); !strings.HasPrefix(got, "text/csv") {
		t.Errorf("content_type = %q, want text/csv", got)
	}

	dest := filepath.Join(e.Work, "downloaded.csv")
	out := decodeObject(t, e.mustRun("mail", "attachment", "get", pub(s, "invoice"), "invoice.csv", "-O", dest))
	if got := str(t, out, "path"); got != dest {
		t.Errorf("path = %q, want %q", got, dest)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, s.Attachment) {
		t.Errorf("attachment bytes round-tripped wrong:\ngot  %q\nwant %q", got, s.Attachment)
	}

	// By part path too, straight to stdout.
	part := str(t, rows[0], "part")
	stdout, _, code := e.run("mail", "attachment", "get", pub(s, "invoice"), part, "-O", "-")
	if code != 0 {
		t.Fatalf("attachment get -O -: exit %d", code)
	}
	if stdout != seedAttachmentBody {
		t.Errorf("stdout attachment = %q, want %q", stdout, seedAttachmentBody)
	}
}


// ---------------------------------------------------------------------------
// (d) flag and mailbox writes

func TestMailMarkArchiveTrash(t *testing.T) {
	e, s := setup(t)

	id := pub(s, "unread")
	remote := s.IDs["unread"]

	rows := decodeArray(t, e.mustRun("mail", "mark", id, "--read"))
	if len(rows) != 1 || !boolean(t, rows[0], "ok") || boolean(t, rows[0], "queued") {
		t.Fatalf("mail mark --read rows = %v", rows)
	}
	if m, ok := e.fake.Message(remote); !ok || !m.Keywords["$seen"] {
		t.Errorf("fake keywords after --read = %v, want $seen", m.Keywords)
	}

	e.mustRun("mail", "mark", id, "--flag")
	if m, _ := e.fake.Message(remote); !m.Keywords["$flagged"] {
		t.Errorf("fake keywords after --flag = %v, want $flagged", m.Keywords)
	}
	flagged := decodeArray(t, e.mustRun("mail", "list", "--flagged"))
	if len(flagged) != 1 || str(t, flagged[0], "id") != id {
		t.Errorf("mail list --flagged = %v, want %s", ids(t, flagged), id)
	}

	// Archive takes the message out of the inbox.
	archived := pub(s, "weekly")
	e.mustRun("mail", "archive", archived)
	m, _ := e.fake.Message(s.IDs["weekly"])
	if m.MailboxIDs[jmapfake.MailboxInbox] {
		t.Errorf("archived message still in inbox: %v", m.MailboxIDs)
	}
	if !m.MailboxIDs[jmapfake.MailboxArchive] {
		t.Errorf("archived message not in archive: %v", m.MailboxIDs)
	}

	// Trash replaces every membership with the trash mailbox.
	e.mustRun("mail", "trash", pub(s, "reply"))
	m2, _ := e.fake.Message(s.IDs["reply"])
	if len(m2.MailboxIDs) != 1 || !m2.MailboxIDs[jmapfake.MailboxTrash] {
		t.Errorf("trashed message mailboxes = %v, want only trash", m2.MailboxIDs)
	}

	// And move to a named mailbox.
	e.mustRun("mail", "move", pub(s, "invoice"), "--to", "Work")
	m3, _ := e.fake.Message(s.IDs["invoice"])
	if !m3.MailboxIDs[s.WorkMailbox] {
		t.Errorf("moved message mailboxes = %v, want the Work mailbox %s", m3.MailboxIDs, s.WorkMailbox)
	}
	if m3.MailboxIDs[jmapfake.MailboxInbox] {
		t.Errorf("moved message is still in the inbox: %v", m3.MailboxIDs)
	}
}

// ---------------------------------------------------------------------------
// (e) reply: dry run, real submission, sent copy

func TestMailReply(t *testing.T) {
	e, s := setup(t)

	dry, stderr, code := e.run("mail", "reply", pub(s, "weekly"), "--body", "ok", "--dry-run")
	if code != 0 {
		t.Fatalf("reply --dry-run: exit %d\n%s", code, stderr)
	}
	for _, want := range []string{
		"In-Reply-To: <weekly-1@example.com>",
		"Subject: Re: Weekly report",
		"To: ",
		"alice@example.com",
	} {
		if !strings.Contains(dry, want) {
			t.Errorf("--dry-run output missing %q:\n%s", want, dry)
		}
	}
	if !strings.Contains(dry, "References: ") {
		t.Errorf("--dry-run output has no References header:\n%s", dry)
	}
	if n := len(e.fake.Submissions()); n != 0 {
		t.Fatalf("--dry-run submitted %d messages, want 0", n)
	}

	out := decodeObject(t, e.mustRun("mail", "reply", pub(s, "weekly"), "--body", "ok"))
	if boolean(t, out, "queued") {
		t.Errorf("reply was queued even though the server is up: %v", out)
	}
	subs := e.fake.Submissions()
	if len(subs) != 1 {
		t.Fatalf("submissions = %d, want 1", len(subs))
	}
	if !bytes.Contains(subs[0], []byte("In-Reply-To: <weekly-1@example.com>")) {
		t.Errorf("submitted message has no In-Reply-To:\n%s", subs[0])
	}
	if !bytes.Contains(subs[0], []byte("Subject: Re: Weekly report")) {
		t.Errorf("submitted message subject wrong:\n%s", subs[0])
	}

	// onSuccessUpdateEmail moved the copy into Sent; a sync pulls it in.
	e.mustRun("sync")
	sent := decodeArray(t, e.mustRun("mail", "list", "--mailbox", "sent"))
	if len(sent) != 1 {
		t.Fatalf("mail list --mailbox sent = %d rows, want 1: %v", len(sent), sent)
	}
	if got := str(t, sent[0], "subject"); got != "Re: Weekly report" {
		t.Errorf("sent subject = %q", got)
	}
	all := decodeArray(t, e.mustRun("mail", "list"))
	if len(all) != 6 {
		t.Errorf("mail list after sending = %d rows, want 6", len(all))
	}

	// The original is marked answered once the reply went through.
	orig := decodeObject(t, e.mustRun("mail", "read", pub(s, "weekly")))
	if !boolean(t, orig, "answered") {
		t.Errorf("original not marked answered after a successful reply")
	}
}

// ---------------------------------------------------------------------------
// (f) offline

func TestOfflineWritesQueueAndReadsWork(t *testing.T) {
	e, s := setup(t)

	e.fake.Close()

	stdout, stderr, code := e.run("mail", "mark", pub(s, "weekly"), "--unread")
	if code != 6 {
		t.Fatalf("offline mark: exit %d, want 6\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	rows := decodeArray(t, stdout)
	if len(rows) != 1 || !boolean(t, rows[0], "queued") {
		t.Errorf("offline mark rows = %v, want one queued row", rows)
	}
	if !strings.Contains(stderr, `"queued"`) {
		t.Errorf("stderr has no JSON error envelope with code queued:\n%s", stderr)
	}

	pending := decodeArray(t, e.mustRun("outbox", "list"))
	if len(pending) != 1 {
		t.Fatalf("outbox list = %d rows, want 1: %v", len(pending), pending)
	}
	if got := str(t, pending[0], "state"); got != "pending" {
		t.Errorf("outbox state = %q, want pending", got)
	}
	if got := str(t, pending[0], "kind"); got != "flags" {
		t.Errorf("outbox kind = %q, want flags", got)
	}

	// Reads still work, from the local index.
	list := decodeArray(t, e.mustRun("mail", "list"))
	if len(list) != 5 {
		t.Errorf("offline mail list = %d rows, want 5", len(list))
	}
	msg := decodeObject(t, e.mustRun("mail", "read", pub(s, "weekly")))
	if !boolean(t, msg, "unread") {
		t.Errorf("the optimistic local flag change was not applied")
	}
	if _, _, code := e.run("mail", "search", "invoice"); code != 0 {
		t.Errorf("offline mail search: exit %d, want 0", code)
	}

	// `sync` reports offline rather than spamming errors.
	if _, _, code := e.run("sync"); code != 4 {
		t.Errorf("offline sync: exit %d, want 4", code)
	}
}

// ---------------------------------------------------------------------------
// small helpers

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
