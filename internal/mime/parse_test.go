package mime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lennert/emlcal/internal/model"
)

func load(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return raw
}

func mustParse(t *testing.T, name string) *Parsed {
	t.Helper()
	p, err := Parse(load(t, name))
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return p
}

func TestParseHTMLOnly(t *testing.T) {
	p := mustParse(t, "html_only.eml")

	if p.Subject != "Your invoice is ready" {
		t.Errorf("subject = %q", p.Subject)
	}
	if p.From != (model.Address{Name: "Marketing", Email: "news@example.com"}) {
		t.Errorf("from = %+v", p.From)
	}
	if len(p.To) != 1 || p.To[0].Name != "Doe, Jane" || p.To[0].Email != "jane@example.org" {
		t.Errorf("to = %+v", p.To)
	}
	if !p.HasHTML || p.HTMLPart != "1" {
		t.Errorf("hasHTML=%v htmlPart=%q", p.HasHTML, p.HTMLPart)
	}
	body := p.TextBody
	for _, want := range []string{
		"Hi Jane,",
		"View invoice (https://billing.example.com/inv/42)",
		"Amount EUR 120,00",
		"Due 2025-02-18",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"alert(", "color:red", "<div>", "<p>"} {
		if strings.Contains(body, bad) {
			t.Errorf("body contains %q:\n%s", bad, body)
		}
	}
	// Table rows must stay on their own lines.
	if strings.Contains(body, "Amount EUR 120,00 Due") {
		t.Errorf("table rows merged:\n%s", body)
	}
}

func TestParseNestedRelatedInlineImage(t *testing.T) {
	p := mustParse(t, "related_inline.eml")

	if p.TextBody != "Here is the photo." {
		t.Errorf("body = %q", p.TextBody)
	}
	wantPaths := []string{"1.1.1", "1.1.2", "1.2", "2"}
	if len(p.AllParts) != len(wantPaths) {
		t.Fatalf("got %d parts, want %d: %+v", len(p.AllParts), len(wantPaths), p.AllParts)
	}
	for i, w := range wantPaths {
		if p.AllParts[i].Path != w {
			t.Errorf("part %d path = %q, want %q", i, p.AllParts[i].Path, w)
		}
	}
	if !p.HasHTML || p.HTMLPart != "1.1.2" {
		t.Errorf("html part = %q", p.HTMLPart)
	}
	if len(p.Attachments) != 2 {
		t.Fatalf("attachments = %+v", p.Attachments)
	}
	img := p.Attachments[0]
	if img.Path != "1.2" || img.ContentType != "image/png" || !img.Inline || img.ContentID != "photo1@example.net" {
		t.Errorf("inline image = %+v", img)
	}
	if img.Filename != "pixel.png" {
		t.Errorf("inline filename = %q", img.Filename)
	}
	pdf := p.Attachments[1]
	if pdf.Path != "2" || pdf.Filename != "report.pdf" || pdf.Disposition != "attachment" {
		t.Errorf("pdf = %+v", pdf)
	}

	data, ct, fn, err := PartContent(load(t, "related_inline.eml"), "2")
	if err != nil {
		t.Fatalf("PartContent: %v", err)
	}
	if string(data) != "%PDF-1.4 fake pdf" {
		t.Errorf("pdf content = %q", data)
	}
	if ct != "application/pdf" || fn != "report.pdf" {
		t.Errorf("ct=%q fn=%q", ct, fn)
	}

	if _, _, _, err := PartContent(load(t, "related_inline.eml"), "9.9"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("bad path error = %v, want ErrNotFound", err)
	}
	if _, _, _, err := PartContent(load(t, "related_inline.eml"), ""); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("empty path error = %v, want ErrNotFound", err)
	}
}

func TestParseCharsets(t *testing.T) {
	tests := []struct {
		file        string
		wantSubject string
		wantFrom    model.Address
		wantInBody  []string
	}{
		{
			file:        "latin1.eml",
			wantSubject: "Grüße aus München",
			wantFrom:    model.Address{Name: "René Müller", Email: "rene@example.de"},
			wantInBody:  []string{"Grüße aus München, café und straße.", "René"},
		},
		{
			file:        "cp1252.eml",
			wantSubject: "Curly quotes",
			wantFrom:    model.Address{Name: "Smart Quotes", Email: "sq@example.com"},
			wantInBody:  []string{"He said “hello” — it’s fine…"},
		},
		{
			file:        "unknown_charset.eml",
			wantSubject: "Weird encodings",
			wantFrom:    model.Address{Email: "weird@example.com"},
			wantInBody:  []string{"Café naïve résumé"},
		},
		{
			file:        "base64_body.eml",
			wantSubject: "Vergadering donderdag",
			wantFrom:    model.Address{Name: "Jan Jansen", Email: "jan@example.nl"},
			wantInBody:  []string{"Dit is een bericht in base64.", "Met vriendelijke groet,"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			p := mustParse(t, tc.file)
			if p.Subject != tc.wantSubject {
				t.Errorf("subject = %q, want %q", p.Subject, tc.wantSubject)
			}
			if p.From != tc.wantFrom {
				t.Errorf("from = %+v, want %+v", p.From, tc.wantFrom)
			}
			for _, w := range tc.wantInBody {
				if !strings.Contains(p.TextBody, w) {
					t.Errorf("body %q missing %q", p.TextBody, w)
				}
			}
			if !utf8Valid(p.TextBody) {
				t.Errorf("body is not valid UTF-8")
			}
		})
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == 0xFFFD && !strings.Contains(s, "\uFFFD") {
			return false
		}
	}
	return true
}

func TestParseRFC2231Filename(t *testing.T) {
	p := mustParse(t, "rfc2231_filename.eml")
	if len(p.Attachments) != 1 {
		t.Fatalf("attachments = %+v", p.Attachments)
	}
	if got, want := p.Attachments[0].Filename, "été jaarrekening 2024 definitief.xlsx"; got != want {
		t.Errorf("filename = %q, want %q", got, want)
	}
	if p.TextBody != "See attached." {
		t.Errorf("body = %q", p.TextBody)
	}
}

func TestParseBrokenMessage(t *testing.T) {
	p := mustParse(t, "broken.eml")

	if p.MessageID != "broken-007@example.com" {
		t.Errorf("message-id = %q", p.MessageID)
	}
	if p.From != (model.Address{Name: "Support", Email: "support@example.com"}) {
		t.Errorf("from = %+v", p.From)
	}
	if len(p.To) != 1 || p.To[0].Name != "Someone" || p.To[0].Email != "" {
		t.Errorf("to = %+v", p.To)
	}
	want := time.Date(2025, 2, 9, 16, 45, 0, 0, time.UTC)
	if !p.Date.Equal(want) {
		t.Errorf("date = %v, want %v", p.Date, want)
	}
	if !strings.Contains(p.TextBody, "Thanks for writing in.") {
		t.Errorf("body = %q", p.TextBody)
	}
}

func TestParseTruncatedMultipart(t *testing.T) {
	p := mustParse(t, "truncated.eml")
	if !strings.Contains(p.TextBody, "The body starts here") {
		t.Errorf("body = %q", p.TextBody)
	}
	if len(p.AllParts) != 1 || p.AllParts[0].Path != "1" {
		t.Errorf("parts = %+v", p.AllParts)
	}
}

func TestParseBulk(t *testing.T) {
	p := mustParse(t, "bulk.eml")
	if !p.IsBulk {
		t.Error("IsBulk = false")
	}
	if p.ListID != "Example Weekly <weekly.list.example.com>" {
		t.Errorf("list-id = %q", p.ListID)
	}
	if p.AutoSubmitted != "auto-generated" || p.Precedence != "bulk" {
		t.Errorf("auto=%q prec=%q", p.AutoSubmitted, p.Precedence)
	}
}

func TestIsBulkRules(t *testing.T) {
	tests := []struct {
		name   string
		hdr    string
		isBulk bool
	}{
		{"plain", "", false},
		{"list-id", "List-Id: <l.example.com>\r\n", true},
		{"auto-generated", "Auto-Submitted: auto-generated\r\n", true},
		{"auto-replied", "Auto-Submitted: auto-replied\r\n", true},
		{"auto-no", "Auto-Submitted: no\r\n", false},
		{"precedence-bulk", "Precedence: bulk\r\n", true},
		{"precedence-list", "Precedence: list\r\n", true},
		{"precedence-junk", "Precedence: junk\r\n", true},
		{"precedence-normal", "Precedence: normal\r\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := "From: a@b.com\r\nSubject: x\r\n" + tc.hdr + "\r\nbody\r\n"
			p, err := Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if p.IsBulk != tc.isBulk {
				t.Errorf("IsBulk = %v, want %v", p.IsBulk, tc.isBulk)
			}
		})
	}
}

func TestParseThreadingHeaders(t *testing.T) {
	raw := "From: a@b.com\r\n" +
		"Message-ID: <c@d.com>\r\n" +
		"In-Reply-To: <parent@d.com>\r\n" +
		"References: <root@d.com> <mid@d.com>\r\n\t<parent@d.com>\r\n" +
		"Subject: =?utf-8?Q?caf=C3=A9?=\r\n\r\nhi\r\n"
	p, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if p.MessageID != "c@d.com" {
		t.Errorf("message-id = %q", p.MessageID)
	}
	if p.InReplyTo != "parent@d.com" {
		t.Errorf("in-reply-to = %q", p.InReplyTo)
	}
	want := []string{"root@d.com", "mid@d.com", "parent@d.com"}
	if len(p.References) != 3 {
		t.Fatalf("references = %v", p.References)
	}
	for i := range want {
		if p.References[i] != want[i] {
			t.Errorf("references[%d] = %q, want %q", i, p.References[i], want[i])
		}
	}
	if p.Subject != "café" {
		t.Errorf("subject = %q", p.Subject)
	}
}

func TestParseDateFallbacks(t *testing.T) {
	tests := []struct {
		in   string
		want time.Time
	}{
		{"Tue, 04 Feb 2025 09:12:33 +0100", time.Date(2025, 2, 4, 9, 12, 33, 0, time.FixedZone("", 3600))},
		{"Tue, 4 Feb 2025 09:12:33 +0100 (CET)", time.Date(2025, 2, 4, 9, 12, 33, 0, time.FixedZone("", 3600))},
		{"4 Feb 2025 09:12:33 +0100", time.Date(2025, 2, 4, 9, 12, 33, 0, time.FixedZone("", 3600))},
		{"Tue, 4 Feb 2025 09:12:33 GMT", time.Date(2025, 2, 4, 9, 12, 33, 0, time.UTC)},
		{"Tue, 4 Feb 2025 09:12:33", time.Date(2025, 2, 4, 9, 12, 33, 0, time.UTC)},
		{"Tue Feb  4 09:12:33 2025", time.Date(2025, 2, 4, 9, 12, 33, 0, time.UTC)},
		{"2025-02-04 09:12:33 +0100", time.Date(2025, 2, 4, 9, 12, 33, 0, time.FixedZone("", 3600))},
		{"Tue, 4 Feb 2025 09:12:33 0100", time.Date(2025, 2, 4, 9, 12, 33, 0, time.FixedZone("", 3600))},
		{"", time.Time{}},
		{"not a date at all", time.Time{}},
	}
	for _, tc := range tests {
		got := parseDate(tc.in)
		if !got.Equal(tc.want) {
			t.Errorf("parseDate(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseAddressListLenient(t *testing.T) {
	tests := []struct {
		in   string
		want []model.Address
	}{
		{`Jane Doe <jane@example.com>`, []model.Address{{Name: "Jane Doe", Email: "jane@example.com"}}},
		{`"Doe, Jane" <jane@example.com>, bob@example.com`, []model.Address{
			{Name: "Doe, Jane", Email: "jane@example.com"},
			{Email: "bob@example.com"},
		}},
		{`"Support" support@example.com`, []model.Address{{Name: "Support", Email: "support@example.com"}}},
		{`Jan <jan@x.nl>; Piet <piet@x.nl>`, []model.Address{
			{Name: "Jan", Email: "jan@x.nl"},
			{Name: "Piet", Email: "piet@x.nl"},
		}},
		{`Someone`, []model.Address{{Name: "Someone"}}},
		{`=?utf-8?Q?Ren=C3=A9?= <rene@x.de>`, []model.Address{{Name: "René", Email: "rene@x.de"}}},
		{``, nil},
	}
	for _, tc := range tests {
		got := parseAddressList(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("parseAddressList(%q) = %+v, want %+v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseAddressList(%q)[%d] = %+v, want %+v", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestParseEmpty(t *testing.T) {
	if _, err := Parse(nil); !errors.Is(err, ErrEmpty) {
		t.Errorf("Parse(nil) err = %v, want ErrEmpty", err)
	}
	if _, err := Parse([]byte("   \r\n  ")); !errors.Is(err, ErrEmpty) {
		t.Errorf("Parse(blank) err = %v, want ErrEmpty", err)
	}
}

func TestParseHeaderlessBody(t *testing.T) {
	p, err := Parse([]byte("just some text, no headers at all\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(p.TextBody, "just some text") {
		t.Errorf("body = %q", p.TextBody)
	}
}

func TestNormalizeText(t *testing.T) {
	in := "line one   \r\nline two\r\n\r\n\r\n\r\n\r\nline three\r\n\r\n"
	want := "line one\nline two\n\n\nline three"
	if got := normalizeText(in); got != want {
		t.Errorf("normalizeText = %q, want %q", got, want)
	}
}

func TestSnippet(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"hello   world\n\nagain", 40, "hello world again"},
		{"hello world", 5, "hello…"},
		{"hello world", 6, "hello…"},
		{"", 10, ""},
		{"anything", 0, ""},
		{"héllo wörld", 7, "héllo w…"},
		{"exact", 5, "exact"},
	}
	for _, tc := range tests {
		if got := Snippet(tc.in, tc.n); got != tc.want {
			t.Errorf("Snippet(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestAttachmentClassification(t *testing.T) {
	raw := "MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--b\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"notes.txt\"\r\n\r\nnotes\r\n" +
		"--b\r\nContent-Type: application/zip; name=\"x.zip\"\r\n\r\nzip\r\n" +
		"--b\r\nContent-Type: image/gif\r\nContent-ID: <img@x>\r\n\r\ngif\r\n" +
		"--b--\r\n"
	p, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if p.TextBody != "body" {
		t.Errorf("body = %q", p.TextBody)
	}
	if len(p.Attachments) != 3 {
		t.Fatalf("attachments = %+v", p.Attachments)
	}
	if p.Attachments[0].Path != "2" || p.Attachments[0].Filename != "notes.txt" {
		t.Errorf("att0 = %+v", p.Attachments[0])
	}
	if p.Attachments[1].Path != "3" || p.Attachments[1].Filename != "x.zip" {
		t.Errorf("att1 = %+v", p.Attachments[1])
	}
	if p.Attachments[2].Path != "4" || !p.Attachments[2].Inline || p.Attachments[2].ContentID != "img@x" {
		t.Errorf("att2 = %+v", p.Attachments[2])
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "passwd"},
		{`C:\Users\bob\x.doc`, "x.doc"},
		{"bad\x00name.txt", "badname.txt"},
		{"..", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := sanitizeFilename(tc.in); got != tc.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseCorpusNeverPanics(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.eml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus: %v", err)
	}
	for _, f := range files {
		raw := load(t, filepath.Base(f))
		p, err := Parse(raw)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if len(p.AllParts) == 0 {
			t.Errorf("%s: no parts", f)
		}
		for _, part := range p.AllParts {
			if part.Path == "" {
				t.Errorf("%s: part with empty path", f)
			}
			if _, _, _, err := PartContent(raw, part.Path); err != nil {
				t.Errorf("%s: PartContent(%q): %v", f, part.Path, err)
			}
		}
	}
}
