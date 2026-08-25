// The assertions in this file come from the 2026-08-25 adversarial review of
// internal/mime (docs/reviews/2026-08-25-data-path.md). They were gated behind
// EMLCAL_REVIEW while the findings were open; M4, M5, M6 and L1 are fixed, so
// they now run as ordinary regression tests.
package mime

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lennert/emlcal/internal/model"
)

func crlf(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

// TestReviewLargeBase64Attachment: a 10 MB attachment must parse in linear time.
func TestReviewLargeBase64Attachment(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 10<<20)
	var b strings.Builder
	b.WriteString("From: a@example.com\nSubject: big\nMIME-Version: 1.0\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=BB\n\n--BB\n")
	b.WriteString("Content-Type: text/plain\n\nhello\n\n--BB\n")
	b.WriteString("Content-Type: application/octet-stream; name=\"big.bin\"\n")
	b.WriteString("Content-Disposition: attachment; filename=\"big.bin\"\n")
	b.WriteString("Content-Transfer-Encoding: base64\n\n")
	raw := append(crlf(b.String()), encodeB64Lines(payload)...)
	raw = append(raw, crlf("\n--BB--\n")...)

	start := time.Now()
	p, err := Parse(raw)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if took > 10*time.Second {
		t.Errorf("parsing a 10 MB base64 attachment took %s", took)
	}
	if len(p.Attachments) != 1 {
		t.Errorf("10 MB attachment: got %d attachments, want 1 (%+v)", len(p.Attachments), p.AllParts)
	} else if p.Attachments[0].Size != int64(len(payload)) {
		t.Errorf("attachment size = %d, want the decoded %d", p.Attachments[0].Size, len(payload))
	}
	if !strings.Contains(p.TextBody, "hello") {
		t.Errorf("body lost next to a big attachment: %q", p.TextBody)
	}
	t.Logf("10 MB attachment parsed in %s", took)
}

func encodeB64Lines(data []byte) []byte {
	const enc = "QUFB" // not used; real encoding below
	_ = enc
	var out bytes.Buffer
	s := b64(data)
	for len(s) > 76 {
		out.WriteString(s[:76] + "\r\n")
		s = s[76:]
	}
	out.WriteString(s + "\r\n")
	return out.Bytes()
}

func b64(data []byte) string {
	var sb strings.Builder
	if err := writeBase64(&sb, data); err != nil {
		panic(err)
	}
	return strings.ReplaceAll(sb.String(), "\r\n", "")
}

// TestReviewDeepNesting: 50 levels of multipart must not blow up or lose the body.
func TestReviewDeepNesting(t *testing.T) {
	const depth = 50
	body := "the innermost body text\n"
	inner := "Content-Type: text/plain\n\n" + body
	for i := depth; i >= 1; i-- {
		bnd := fmt.Sprintf("B%d", i)
		inner = fmt.Sprintf("Content-Type: multipart/mixed; boundary=%s\n\n--%s\n%s\n--%s--\n", bnd, bnd, inner, bnd)
	}
	raw := crlf("From: a@example.com\nSubject: deep\nMIME-Version: 1.0\n" + inner)

	done := make(chan *Parsed, 1)
	go func() {
		p, err := Parse(raw)
		if err != nil {
			t.Errorf("Parse: %v", err)
		}
		done <- p
	}()
	select {
	case p := <-done:
		if p == nil {
			return
		}
		if !strings.Contains(p.TextBody, "innermost") {
			t.Errorf("depth-%d multipart: body not extracted (maxDepth=%d); TextBody=%q parts=%d",
				depth, maxDepth, p.TextBody, len(p.AllParts))
		}
	case <-time.After(20 * time.Second):
		t.Errorf("Parse hung on a depth-%d multipart", depth)
	}
}

// TestReviewMessageRFC822Attachment: a forwarded message part.
func TestReviewMessageRFC822Attachment(t *testing.T) {
	inner := "From: b@example.com\nSubject: inner\n\ninner body\n"
	for _, tc := range []struct {
		name, headers string
		wantAtt       bool
	}{
		{"with disposition", "Content-Type: message/rfc822\nContent-Disposition: attachment; filename=\"fwd.eml\"\n", true},
		{"bare", "Content-Type: message/rfc822\n", true},
	} {
		raw := crlf("From: a@example.com\nSubject: fwd\nMIME-Version: 1.0\n" +
			"Content-Type: multipart/mixed; boundary=BB\n\n--BB\n" +
			"Content-Type: text/plain\n\nsee attached\n\n--BB\n" + tc.headers + "\n" + inner + "\n--BB--\n")
		p, err := Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !strings.Contains(p.TextBody, "see attached") {
			t.Errorf("%s: outer body lost: %q", tc.name, p.TextBody)
		}
		if got := len(p.Attachments) > 0; got != tc.wantAtt {
			t.Errorf("%s: message/rfc822 recorded as attachment = %v, want %v (parts: %+v)",
				tc.name, got, tc.wantAtt, p.AllParts)
		}
		if strings.Contains(p.TextBody, "inner body") {
			t.Errorf("%s: the attached message's body leaked into text_body: %q", tc.name, p.TextBody)
		}
	}
}

// TestReviewTextPlainAsAttachment: text/plain marked attachment must not become
// the display body.
func TestReviewTextPlainAsAttachment(t *testing.T) {
	raw := crlf("From: a@example.com\nSubject: s\nMIME-Version: 1.0\n" +
		"Content-Type: multipart/mixed; boundary=BB\n\n--BB\n" +
		"Content-Type: text/plain; charset=utf-8\nContent-Disposition: attachment; filename=\"notes.txt\"\n\nATTACHED NOTES\n\n--BB\n" +
		"Content-Type: text/plain; charset=utf-8\n\nreal body\n\n--BB--\n")
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.TextBody, "ATTACHED NOTES") {
		t.Errorf("an attached text/plain became the display body: %q", p.TextBody)
	}
	if !strings.Contains(p.TextBody, "real body") {
		t.Errorf("the real body was not selected: %q", p.TextBody)
	}
	if len(p.Attachments) != 1 || p.Attachments[0].Filename != "notes.txt" {
		t.Errorf("attached text/plain not recorded: %+v", p.Attachments)
	}
}

// TestReviewOddHeaders: missing Content-Type, LF-only, 8bit UTF-8 without a
// charset, mixed encoded words, quoted names with commas, group syntax.
func TestReviewOddHeaders(t *testing.T) {
	t.Run("no content-type, LF only", func(t *testing.T) {
		p, err := Parse([]byte("From: a@example.com\nSubject: plain\n\njust a body\n"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.TextBody, "just a body") {
			t.Errorf("LF-only message with no Content-Type lost its body: %q", p.TextBody)
		}
		if p.Subject != "plain" {
			t.Errorf("subject = %q, want %q", p.Subject, "plain")
		}
	})

	t.Run("8bit utf-8 no charset", func(t *testing.T) {
		p, err := Parse(crlf("From: a@example.com\nSubject: s\nContent-Type: text/plain\n\nvoilà — naïve café\n"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.TextBody, "voilà — naïve café") {
			t.Errorf("8bit UTF-8 without charset mangled: %q", p.TextBody)
		}
	})

	t.Run("mixed encoded-word subject", func(t *testing.T) {
		raw := crlf("From: a@example.com\nSubject: =?utf-8?q?Caf=C3=A9?= meeting =?iso-8859-1?B?bel9?= end\n\nbody\n")
		p, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.Subject, "Café") || !strings.Contains(p.Subject, "meeting") || !strings.HasSuffix(p.Subject, "end") {
			t.Errorf("mixed encoded-word subject = %q", p.Subject)
		}
	})

	t.Run("quoted name with comma", func(t *testing.T) {
		raw := crlf("From: a@example.com\nTo: \"Doe, John\" <john@example.com>, \"Roe; Jane\" <jane@example.com>\nSubject: s\n\nbody\n")
		p, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.To) != 2 {
			t.Errorf("quoted names with commas split into %d recipients: %+v", len(p.To), p.To)
		} else {
			if p.To[0].Email != "john@example.com" || p.To[0].Name != "Doe, John" {
				t.Errorf("first recipient = %+v", p.To[0])
			}
			if p.To[1].Email != "jane@example.com" {
				t.Errorf("second recipient = %+v", p.To[1])
			}
		}
	})

	t.Run("group address", func(t *testing.T) {
		raw := crlf("From: a@example.com\nTo: Team:john@example.com,jane@example.com;\nSubject: s\n\nbody\n")
		p, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		var emails []string
		for _, a := range p.To {
			if a.Email != "" {
				emails = append(emails, a.Email)
			}
		}
		if len(emails) != 2 {
			t.Errorf("group address yielded %v, want both members: %+v", emails, p.To)
		}
	})

	t.Run("undisclosed recipients", func(t *testing.T) {
		raw := crlf("From: a@example.com\nTo: undisclosed-recipients:;\nSubject: s\n\nbody\n")
		if _, err := Parse(raw); err != nil {
			t.Errorf("Parse failed on undisclosed-recipients: %v", err)
		}
	})
}

// TestReviewHTMLStyleAndTables checks that <style> is dropped and tables read.
func TestReviewHTMLStyleAndTables(t *testing.T) {
	html := `<html><head><style type="text/css">
	.x { color: red; font-family: "Helvetica Neue"; }
	@media screen { .y { display:none } }
	</style></head><body>
	<p>Hello there</p>
	<table><tr><td>Item</td><td>Qty</td></tr><tr><td>Widget</td><td>3</td></tr></table>
	<a href="https://example.com/x">click here</a>
	</body></html>`
	raw := crlf("From: a@example.com\nSubject: s\nMIME-Version: 1.0\nContent-Type: text/html; charset=utf-8\n\n" + html + "\n")
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.TextBody, "color: red") || strings.Contains(p.TextBody, "@media") {
		t.Errorf("<style> content leaked into text_body:\n%s", p.TextBody)
	}
	if !strings.Contains(p.TextBody, "Hello there") {
		t.Errorf("HTML body text lost:\n%s", p.TextBody)
	}
	for _, want := range []string{"Item", "Widget"} {
		if !strings.Contains(p.TextBody, want) {
			t.Errorf("table cell %q missing from:\n%s", want, p.TextBody)
		}
	}
	// Rows must not be glued into one line.
	if strings.Contains(p.TextBody, "QtyWidget") {
		t.Errorf("table rows glued together:\n%s", p.TextBody)
	}
	if !strings.Contains(p.TextBody, "https://example.com/x") {
		t.Errorf("link target dropped:\n%s", p.TextBody)
	}
}

// TestReviewStripQuotesFalsePositives is the interesting half of §8.
func TestReviewStripQuotesFalsePositives(t *testing.T) {
	cases := []struct {
		name, in, mustKeep string
	}{
		{
			"paragraph ending in wrote:",
			"Quick update before the call.\n\nOn the subject of the vendor contract, here is what their lawyer wrote:\n\nWe cannot accept clause 4.\n\nThat is the sticking point, so please read it before Friday.\n",
			"cannot accept clause 4",
		},
		{
			"python repl in the body",
			"Here is the repro:\n\n>>> import emlcal\n>>> emlcal.sync()\nTraceback: boom\n\nAny ideas?\n",
			"import emlcal",
		},
		{
			"markdown quote the sender typed",
			"The spec says:\n\n> messages MUST be idempotent\n\nbut our code is not.\n",
			"messages MUST be idempotent",
		},
		{
			"-- inside a list of options",
			"Options:\n\n--\nA) do nothing\nB) rewrite it\n\nWhich one?\n",
			"rewrite it",
		},
		{
			"shell flags on their own line",
			"Run it like this:\n\n--verbose\n--dry-run\n\nthen check the log.\n",
			"then check the log",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StripQuotes(tc.in)
			if !strings.Contains(got, tc.mustKeep) {
				t.Errorf("StripQuotes dropped %q.\n--- in ---\n%s\n--- out ---\n%s", tc.mustKeep, tc.in, got)
			}
		})
	}
}

// TestReviewStripQuotesTruePositives makes sure the heuristics still work.
func TestReviewStripQuotesTruePositives(t *testing.T) {
	in := "Sounds good, thanks.\n\nOn Mon, 3 Feb 2025 at 09:14, Jane Doe <jane@example.com> wrote:\n> the original question\n> second line\n"
	got := StripQuotes(in)
	if strings.Contains(got, "original question") {
		t.Errorf("quoted reply not stripped: %q", got)
	}
	if !strings.Contains(got, "Sounds good") {
		t.Errorf("reply text lost: %q", got)
	}
}

// TestReviewBuildHeaderInjection: no user-supplied field may create a header.
func TestReviewBuildHeaderInjection(t *testing.T) {
	d := &Draft{
		From:     model.Address{Name: "Ann\r\nX-Evil: from-name", Email: "ann@example.com"},
		To:       []model.Address{{Name: "Bob", Email: "bob@example.com\r\nBcc: victim@example.com"}},
		Subject:  "Hello\r\nBcc: attacker@example.com\r\nX-Evil: yes",
		TextBody: "body\n",
		Date:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	raw, err := Build(d)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	headerEnd := bytes.Index(raw, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		t.Fatalf("no header/body separator in:\n%s", raw)
	}
	head := string(raw[:headerEnd])
	for _, bad := range []string{"\r\nBcc:", "\r\nX-Evil:", "\r\n\r\n"} {
		if strings.Contains(head, bad) {
			t.Errorf("header injection: %q appears in the header block:\n%s", bad, head)
		}
	}
	// Round-trip: the parsed message must not have gained recipients.
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Bcc) != 0 {
		t.Errorf("injected Bcc survived the round-trip: %+v", p.Bcc)
	}
	if len(p.To) != 1 {
		t.Errorf("injected To produced %d recipients: %+v", len(p.To), p.To)
	}
}

// TestReviewBuildLineLength: Build documents "no line longer than 998 bytes".
func TestReviewBuildLineLength(t *testing.T) {
	long := strings.Repeat("x", 2000) // a single unbreakable token
	for _, tc := range []struct{ name, subject string }{
		{"long unbroken subject", long},
		{"long url subject", "Re: see https://example.com/" + strings.Repeat("a", 1500)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := Build(&Draft{
				From: model.Address{Email: "a@example.com"}, To: []model.Address{{Email: "b@example.com"}},
				Subject: tc.subject, TextBody: "hi\n", Date: time.Unix(0, 0).UTC(),
			})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			for i, line := range strings.Split(string(raw), "\r\n") {
				if len(line) > 998 {
					t.Errorf("line %d is %d bytes (RFC 5322 limit is 998): %.80s…", i, len(line), line)
					break
				}
			}
		})
	}
}

// TestReviewBuildNonASCIIDisplayName encodes a name with accents and commas.
func TestReviewBuildNonASCIIDisplayName(t *testing.T) {
	raw, err := Build(&Draft{
		From:     model.Address{Name: "Renée Müller", Email: "renee@example.com"},
		To:       []model.Address{{Name: "Doe, John", Email: "john@example.com"}},
		Subject:  "Überraschung",
		TextBody: "hallo\n",
		Date:     time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.From.Name != "Renée Müller" {
		t.Errorf("non-ASCII display name round-trip: %q", p.From.Name)
	}
	if p.Subject != "Überraschung" {
		t.Errorf("non-ASCII subject round-trip: %q", p.Subject)
	}
	if len(p.To) != 1 || p.To[0].Name != "Doe, John" || p.To[0].Email != "john@example.com" {
		t.Errorf("display name with a comma round-trip: %+v", p.To)
	}
}

// TestReviewCRLFvsLF parses the same message with both line endings.
func TestReviewCRLFvsLF(t *testing.T) {
	src := "From: a@example.com\nSubject: s\nMIME-Version: 1.0\n" +
		"Content-Type: multipart/alternative; boundary=BB\n\n--BB\n" +
		"Content-Type: text/plain\n\nplain body\n\n--BB\n" +
		"Content-Type: text/html\n\n<p>html body</p>\n\n--BB--\n"
	lf, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	cr, err := Parse(crlf(src))
	if err != nil {
		t.Fatal(err)
	}
	if lf.TextBody != cr.TextBody {
		t.Errorf("LF and CRLF forms differ:\nLF:   %q\nCRLF: %q", lf.TextBody, cr.TextBody)
	}
	if len(lf.AllParts) != len(cr.AllParts) {
		t.Errorf("LF found %d parts, CRLF found %d", len(lf.AllParts), len(cr.AllParts))
	}
}
