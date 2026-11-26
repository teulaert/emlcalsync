package cli

import (
	"strings"
	"testing"

	"github.com/teulaert/emlcalsync/internal/mime"
)

// sentHTML parses a message that went out and returns its text body and the
// HTML alternative next to it (EML-4: a text/plain-only mail is shown in a
// narrow column by Apple Mail and Outlook).
func sentHTML(t *testing.T, raw []byte) (text, html string) {
	t.Helper()
	p, err := mime.Parse(raw)
	if err != nil {
		t.Fatalf("what went out does not parse: %v", err)
	}
	if !p.HasHTML {
		t.Fatalf("what went out has no HTML alternative:\n%s", raw)
	}
	data, _, _, err := mime.PartContent(raw, p.HTMLPart)
	if err != nil {
		t.Fatalf("PartContent(html): %v", err)
	}
	return p.TextBody, string(data)
}

func TestMailSendCarriesAnHTMLAlternative(t *testing.T) {
	env := mailSeed(t)
	long := strings.Repeat("one long paragraph without hard breaks ", 10)
	env.MustRun("mail", "send", "--account", "work", "--to", "x@y.example", "--subject", "s",
		"--body", long+"\n\nsecond paragraph")

	sent := env.Mail["work"].Sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %d, want 1", len(sent))
	}
	text, html := sentHTML(t, sent[0])
	if !strings.Contains(text, strings.TrimSpace(long)) {
		t.Errorf("text body lost the paragraph: %q", text)
	}
	if !strings.Contains(html, "<p>"+strings.TrimSpace(long)+"</p>") || !strings.Contains(html, "<p>second paragraph</p>") {
		t.Errorf("html:\n%s", html)
	}
}

func TestMailReplyQuotesAsBlockquoteInHTML(t *testing.T) {
	env := mailSeed(t)
	env.MustRun("mail", "reply", "work:m-unread", "--body", "thanks")

	sent := env.Mail["work"].Sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %d, want 1", len(sent))
	}
	text, html := sentHTML(t, sent[0])
	if !strings.Contains(text, "> hello, the numbers are attached") {
		t.Errorf("text quote missing: %q", text)
	}
	i := strings.Index(html, "<p>thanks</p>")
	j := strings.Index(html, "<blockquote type=\"cite\">")
	k := strings.Index(html, "hello, the numbers are attached")
	if i < 0 || j < i || k < j {
		t.Errorf("want the answer, then the original inside a blockquote:\n%s", html)
	}
}

func TestMailForwardCarriesAnHTMLAlternativeAndTheFile(t *testing.T) {
	env := mailSeed(t)
	env.MustRun("mail", "forward", "work:m-unread", "--to", "jan@example.com", "--body", "fyi")

	sent := env.Mail["work"].Sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %d, want 1", len(sent))
	}
	_, html := sentHTML(t, sent[0])
	if !strings.Contains(html, "<p>fyi</p>") || !strings.Contains(html, "Forwarded message") ||
		!strings.Contains(html, "hello, the numbers are attached") {
		t.Errorf("html:\n%s", html)
	}
	p, _ := mime.Parse(sent[0])
	if len(p.Attachments) != 1 || p.Attachments[0].Filename != "report.csv" {
		t.Errorf("attachments = %+v, want report.csv (and the html part not counted as one)", p.Attachments)
	}
}

// A send queued while offline goes out later with the same alternative: the
// outbox holds the built message.
func TestMailSendQueuedKeepsTheHTMLAlternative(t *testing.T) {
	env := mailSeed(t)
	env.Mail["work"].FailNext(1)
	_, errs, code := env.Run("mail", "send", "--account", "work", "--to", "x@y.example", "--subject", "s", "--body", "later")
	if code != 6 {
		t.Fatalf("exit = %d, want 6 (queued)\n%s", code, errs)
	}
	env.MustRun("outbox", "retry")
	sent := env.Mail["work"].Sent()
	if len(sent) != 1 {
		t.Fatalf("sent after retry = %d, want 1", len(sent))
	}
	if _, html := sentHTML(t, sent[0]); !strings.Contains(html, "<p>later</p>") {
		t.Errorf("html:\n%s", html)
	}
}
