package mime

import (
	"bytes"
	"io"
	stdmime "mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"

	"github.com/teulaert/emlcalsync/internal/model"
)

func TestTextToHTML(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string // substrings, in this order
		not      []string
	}{
		{
			name: "a long paragraph is one paragraph, unwrapped",
			in:   strings.Repeat("word ", 60) + "end.",
			want: []string{"<p>" + strings.Repeat("word ", 60) + "end.</p>"},
			not:  []string{"<br>"},
		},
		{
			name: "blank lines separate paragraphs, single newlines are breaks",
			in:   "Hi Jan,\n\nline one\nline two\n\nBye",
			want: []string{"<p>Hi Jan,</p>", "<p>line one<br>\nline two</p>", "<p>Bye</p>"},
		},
		{
			name: "markup in the text is escaped",
			in:   `a <b>bold</b> & "quoted" claim`,
			want: []string{"<p>a &lt;b&gt;bold&lt;/b&gt; &amp; &#34;quoted&#34; claim</p>"},
			not:  []string{"<b>"},
		},
		{
			name: "URLs are linked, sentence punctuation is not part of them",
			in:   "See https://example.org/a?b=1&c=2. Or (http://x.example/p).",
			want: []string{
				`<a href="https://example.org/a?b=1&amp;c=2">https://example.org/a?b=1&amp;c=2</a>.`,
				`(<a href="http://x.example/p">http://x.example/p</a>).`,
			},
		},
		{
			name: "a parenthesis inside a URL stays",
			in:   "https://en.wikipedia.org/wiki/Go_(language)",
			want: []string{`<a href="https://en.wikipedia.org/wiki/Go_(language)">`},
		},
		{
			name: "quoted lines become nested blockquotes",
			in:   "Sure.\n\nOn Mon, Jan wrote:\n> first\n> > older\n>> also older\n> back\n\nBye",
			want: []string{
				"<p>Sure.</p>",
				"<p>On Mon, Jan wrote:</p>",
				"<blockquote type=\"cite\">\n<p>first</p>",
				"<blockquote type=\"cite\">\n<p>older<br>\nalso older</p>\n</blockquote>",
				"<p>back</p>\n</blockquote>",
				"<p>Bye</p>",
			},
		},
		{
			name: "an indented > is not a quote",
			in:   "  > not a quote",
			want: []string{"<p>&nbsp; &gt; not a quote</p>"},
			not:  []string{"blockquote"},
		},
		{
			name: "runs of spaces and tabs keep their width",
			in:   "a  b\tc",
			want: []string{"<p>a &nbsp;b &nbsp; &nbsp;c</p>"},
		},
		{
			name: "no styling",
			in:   "plain",
			not:  []string{"style", "font", "<div"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TextToHTML(tc.in)
			at := 0
			for _, w := range tc.want {
				i := strings.Index(got[at:], w)
				if i < 0 {
					t.Fatalf("missing (or out of order) %q in\n%s", w, got)
				}
				at += i + len(w)
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("unexpected %q in\n%s", n, got)
				}
			}
			if strings.Count(got, "<blockquote") != strings.Count(got, "</blockquote>") {
				t.Errorf("unbalanced blockquotes in\n%s", got)
			}
		})
	}
}

// A quote that runs to the end of the body is closed.
func TestTextToHTMLClosesTrailingQuotes(t *testing.T) {
	got := TextToHTML("ok\n> a\n> > b")
	if strings.Count(got, "<blockquote") != 2 || strings.Count(got, "</blockquote>") != 2 {
		t.Errorf("blockquotes not closed:\n%s", got)
	}
}

// bodyParts walks a built message and returns its leaf parts' content types
// and decoded bodies, outermost structure as a path such as
// "multipart/mixed>multipart/alternative>text/html".
func bodyParts(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	var walk func(prefix, ctype, cte string, body io.Reader)
	walk = func(prefix, ctype, cte string, body io.Reader) {
		mt, params, err := stdmime.ParseMediaType(ctype)
		if err != nil {
			t.Fatalf("content-type %q: %v", ctype, err)
		}
		path := mt
		if prefix != "" {
			path = prefix + ">" + mt
		}
		if strings.HasPrefix(mt, "multipart/") {
			mr := multipart.NewReader(body, params["boundary"])
			for {
				p, err := mr.NextRawPart()
				if err == io.EOF {
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				walk(path, p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p)
			}
		}
		var r io.Reader = body
		if strings.EqualFold(cte, "quoted-printable") {
			r = quotedprintable.NewReader(body)
		}
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		out[path] = strings.ReplaceAll(string(b), "\r\n", "\n") // the wire is CRLF
	}
	walk("", msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body)
	return out
}

func TestBuildSendsTextAndHTMLAlternative(t *testing.T) {
	body := strings.Repeat("A long paragraph that must flow to the window width. ", 8) + "\n\nSecond <one>."
	raw, err := Build(&Draft{
		From: addr("me@example.com"), To: addrs("you@example.com"),
		Subject: "hi", TextBody: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkWireFormat(t, raw)
	parts := bodyParts(t, raw)
	if len(parts) != 2 {
		t.Fatalf("parts = %v, want text and html only", keys(parts))
	}
	plain, ok := parts["multipart/alternative>text/plain"]
	if !ok || plain != normalizeText(body)+"\n" {
		t.Errorf("text/plain part = %q, want the body unchanged", plain)
	}
	html, ok := parts["multipart/alternative>text/html"]
	if !ok {
		t.Fatalf("no text/html alternative: %v", keys(parts))
	}
	if !strings.Contains(html, "<p>"+strings.TrimSpace(strings.Repeat("A long paragraph that must flow to the window width. ", 8))+"</p>") ||
		!strings.Contains(html, "Second &lt;one&gt;.") {
		t.Errorf("html part:\n%s", html)
	}
	// Text first: an alternative is ordered worst to best.
	if i, j := bytes.Index(raw, []byte("Content-Type: text/plain")), bytes.Index(raw, []byte("Content-Type: text/html")); i < 0 || j < i {
		t.Errorf("text/plain does not come before text/html")
	}
}

func TestBuildWithAttachmentsNestsTheAlternative(t *testing.T) {
	raw, err := Build(&Draft{
		From: addr("me@example.com"), To: addrs("you@example.com"), Subject: "files", TextBody: "see attached",
		Attachments: []DraftAttachment{{Filename: "a.txt", ContentType: "text/plain", Data: []byte("data")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	parts := bodyParts(t, raw)
	for _, k := range []string{
		"multipart/mixed>multipart/alternative>text/plain",
		"multipart/mixed>multipart/alternative>text/html",
		"multipart/mixed>text/plain",
	} {
		if _, ok := parts[k]; !ok {
			t.Errorf("missing %s; have %v", k, keys(parts))
		}
	}
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.TextBody != "see attached" || len(p.Attachments) != 1 || p.Attachments[0].Filename != "a.txt" {
		t.Errorf("parsed back: body %q attachments %+v", p.TextBody, p.Attachments)
	}
}

// An iTIP answer keeps its RFC 6047 shape: text and calendar, no HTML.
func TestBuildCalendarHasNoHTML(t *testing.T) {
	raw, err := Build(&Draft{
		From: addr("me@example.com"), To: addrs("org@example.com"), Subject: "Accepted", TextBody: "accepted",
		Calendar: &DraftCalendar{Method: "REPLY", Content: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	parts := bodyParts(t, raw)
	if _, ok := parts["multipart/alternative>text/calendar"]; !ok || len(parts) != 2 {
		t.Errorf("parts = %v, want text/plain and text/calendar", keys(parts))
	}
	for k := range parts {
		if strings.HasSuffix(k, "text/html") {
			t.Errorf("iTIP message carries %s", k)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func addr(email string) model.Address { return model.Address{Email: email} }

func addrs(emails ...string) []model.Address {
	var out []model.Address
	for _, e := range emails {
		out = append(out, addr(e))
	}
	return out
}
