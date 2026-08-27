package e2e

import (
	"strings"
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/teulaert/emlcalsync/internal/testutil/imapfake"
)

// imapEnv is an env with a fake IMAP and SMTP server wired in, so the real
// binary goes through the real Factory, the real config and the real secrets.
type imapEnv struct {
	*env
	imap *imapfake.Server
	smtp *imapfake.SMTPServer
}

func newIMAPEnv(t *testing.T) *imapEnv {
	t.Helper()
	e := newEnv(t)
	srv := imapfake.New(t)
	srv.CreateMailbox("Archive")
	srv.CreateMailbox("Sent")
	srv.CreateMailbox("Drafts")
	srv.CreateMailbox("Trash")
	smtpSrv := imapfake.NewSMTP(t)

	e.Extra = append(e.Extra,
		"EMLCAL_IMAP_ADDR="+srv.Addr(),
		"EMLCAL_SMTP_ADDR="+smtpSrv.Addr(),
		"EMLCAL_IMAP_INSECURE=1",
	)
	return &imapEnv{env: e, imap: srv, smtp: smtpSrv}
}

// addIMAPAccount creates an IMAP account through the CLI, the way a user would.
func (e *imapEnv) addIMAPAccount(t *testing.T) {
	t.Helper()
	stdout, stderr, code := e.runInput(imapfake.Password+"\n",
		"account", "add", "imap",
		"--name", "home", "--email", imapfake.Username,
		"--host", "127.0.0.1", "--security", "none",
		"--password-stdin")
	if code != 0 {
		t.Fatalf("account add imap: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
}

// firstID is the id of the first row in a JSON list.
func firstID(t *testing.T, out string) string {
	t.Helper()
	rows := decodeArray(t, out)
	if len(rows) == 0 {
		t.Fatalf("no rows in:\n%s", out)
	}
	id, _ := rows[0]["id"].(string)
	if id == "" {
		t.Fatalf("first row has no id: %+v", rows[0])
	}
	return id
}

// countRows is how many rows a JSON list holds.
func countRows(t *testing.T, out string) int {
	t.Helper()
	return len(decodeArray(t, out))
}

// The whole path, through the real binary: add an account, back-fill it, then
// pick up a change with a delta.
func TestIMAPBackfillThenDelta(t *testing.T) {
	e := newIMAPEnv(t)
	e.imap.Mail("INBOX", "first message", "hello from the fake")
	e.imap.Mail("Archive", "an older one", "archived")

	e.addIMAPAccount(t)
	e.mustRun("sync", "--account", "home")

	out := e.mustRun("mail", "list", "--account", "home", "--limit", "50")
	if n := countRows(t, out); n != 2 {
		t.Fatalf("listed %d messages, want 2:\n%s", n, out)
	}
	if !strings.Contains(out, "first message") {
		t.Errorf("the inbox message is missing:\n%s", out)
	}

	// New mail arrives; a delta must find it without a full resync.
	e.imap.Mail("INBOX", "second message", "and again")
	e.mustRun("sync", "--account", "home")

	out = e.mustRun("mail", "list", "--account", "home", "--limit", "50")
	if n := countRows(t, out); n != 3 {
		t.Fatalf("after the delta, listed %d messages, want 3:\n%s", n, out)
	}
}

// Search goes through the FTS index, which only works if the raw bytes were
// archived and parsed.
func TestIMAPSearchFindsBodyText(t *testing.T) {
	e := newIMAPEnv(t)
	e.imap.Mail("INBOX", "invoice", "the zeppelin has landed")
	e.addIMAPAccount(t)
	e.mustRun("sync", "--account", "home")

	out := e.mustRun("mail", "search", "zeppelin", "--account", "home")
	if !strings.Contains(out, "invoice") {
		t.Errorf("search did not find the message:\n%s", out)
	}
}

// Archiving moves the message on the server, which mints a new uid. The id the
// user is handed back must be the one the message now has -- and the message
// must not be re-downloaded.
func TestIMAPArchiveRenamesRatherThanRefetching(t *testing.T) {
	e := newIMAPEnv(t)
	e.imap.Mail("INBOX", "to be filed", "hello")
	e.addIMAPAccount(t)
	e.mustRun("sync", "--account", "home")

	id := firstID(t, e.mustRun("mail", "list", "--account", "home"))
	e.mustRun("mail", "archive", id)

	if e.imap.Count("INBOX") != 0 || e.imap.Count("Archive") != 1 {
		t.Fatalf("server has inbox=%d archive=%d, want the message moved",
			e.imap.Count("INBOX"), e.imap.Count("Archive"))
	}

	// A sync afterwards must settle without duplicating anything.
	e.mustRun("sync", "--account", "home")
	out := e.mustRun("mail", "list", "--account", "home", "--limit", "50")
	if n := countRows(t, out); n != 1 {
		t.Errorf("listed %d messages after archiving one, want 1:\n%s", n, out)
	}
}

// Threading is stitched from headers, since IMAP supplies no thread id.
func TestIMAPThreadsAReplyWithItsParent(t *testing.T) {
	e := newIMAPEnv(t)
	e.imap.AddMessage("INBOX", []byte(imapfake.Message("a question", "what time?")))
	e.imap.AddMessage("INBOX", []byte(imapfake.Reply("a question", "three o'clock", "a-question")))

	e.addIMAPAccount(t)
	e.mustRun("sync", "--account", "home")

	out := e.mustRun("mail", "list", "--account", "home", "--limit", "50")
	if n := countRows(t, out); n != 2 {
		t.Fatalf("listed %d messages, want 2:\n%s", n, out)
	}
	// The reply names its parent in References, so asking for either one's
	// thread must return both -- which only works because the store stitched
	// them: IMAP supplied no thread id.
	id := firstID(t, out)
	thread := decodeObject(t, e.mustRun("mail", "thread", id))
	msgs, _ := thread["messages"].([]any)
	if len(msgs) != 2 {
		t.Errorf("thread of %s holds %d messages, want the parent and the reply:\n%+v",
			id, len(msgs), thread)
	}
}

// Sending goes out over SMTP and the copy is filed in Sent.
func TestIMAPSendGoesOverSMTP(t *testing.T) {
	e := newIMAPEnv(t)
	e.addIMAPAccount(t)
	e.mustRun("sync", "--account", "home")

	e.mustRun("mail", "send", "--account", "home",
		"--to", "someone@example.com", "--bcc", "blind@example.com",
		"--subject", "hello", "--body", "a message")

	sent := e.smtp.Sent()
	if len(sent) != 1 {
		t.Fatalf("submitted %d messages, want 1", len(sent))
	}
	// The reason Submitter exists: Bcc is not in the message, so only the
	// envelope can carry it.
	if !sent[0].HasRecipient("blind@example.com") {
		t.Errorf("RCPT TO = %v, missing the blind recipient", sent[0].To)
	}
	if strings.Contains(strings.ToLower(sent[0].Body()), "bcc:") {
		t.Error("the Bcc header reached the recipients")
	}
	if e.imap.Count("Sent") != 1 {
		t.Errorf("Sent holds %d messages, want the filed copy", e.imap.Count("Sent"))
	}
}

// doctor is the first thing anyone runs against a server emlcal has not met.
func TestIMAPDoctorReportsCapabilities(t *testing.T) {
	e := newIMAPEnv(t)
	e.addIMAPAccount(t)

	out := e.mustRun("doctor")
	if !strings.Contains(out, "UIDPLUS") || !strings.Contains(out, "MOVE") {
		t.Errorf("doctor did not report what the server supports:\n%s", out)
	}
}

// A server without MOVE must still be able to archive.
func TestIMAPWorksWithoutMoveCapability(t *testing.T) {
	e := newEnv(t)
	srv := imapfake.New(t, imapfake.HideCaps(imapv2.CapMove))
	srv.CreateMailbox("Archive")
	smtpSrv := imapfake.NewSMTP(t)
	e.Extra = append(e.Extra,
		"EMLCAL_IMAP_ADDR="+srv.Addr(),
		"EMLCAL_SMTP_ADDR="+smtpSrv.Addr(),
		"EMLCAL_IMAP_INSECURE=1")
	ie := &imapEnv{env: e, imap: srv, smtp: smtpSrv}

	srv.Mail("INBOX", "the long way round", "hello")
	ie.addIMAPAccount(t)
	ie.mustRun("sync", "--account", "home")

	id := firstID(t, ie.mustRun("mail", "list", "--account", "home"))
	ie.mustRun("mail", "archive", id)
	if srv.Count("Archive") != 1 {
		t.Errorf("archive holds %d, want the copy", srv.Count("Archive"))
	}
}
