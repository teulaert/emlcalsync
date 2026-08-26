package mime

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

func checkWireFormat(t *testing.T, raw []byte) {
	t.Helper()
	if bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) {
		t.Error("message contains a bare LF")
	}
	for i, line := range bytes.Split(raw, []byte("\r\n")) {
		if len(line) > 998 {
			t.Errorf("line %d is %d bytes, want <= 998", i+1, len(line))
		}
	}
	if !bytes.Contains(raw, []byte("\r\n\r\n")) {
		t.Error("no header/body separator")
	}
}

func TestBuildSimple(t *testing.T) {
	d := &Draft{
		From:      model.Address{Name: "Bob Smith", Email: "bob@example.com"},
		To:        []model.Address{{Name: "Jane Doe", Email: "jane@example.org"}},
		Cc:        []model.Address{{Email: "team@example.org"}},
		Subject:   "Lunch on Thursday",
		TextBody:  "Hi Jane,\n\nDoes Thursday work?\n\nBob",
		Date:      time.Date(2025, 2, 3, 9, 14, 0, 0, time.FixedZone("CET", 3600)),
		MessageID: "abc123@example.com",
	}
	raw, err := Build(d)
	if err != nil {
		t.Fatal(err)
	}
	checkWireFormat(t, raw)

	s := string(raw)
	for _, want := range []string{
		"Date: Mon, 03 Feb 2025 09:14:00 +0100\r\n",
		"From: \"Bob Smith\" <bob@example.com>\r\n",
		"To: \"Jane Doe\" <jane@example.org>\r\n",
		"Cc: <team@example.org>\r\n",
		"Subject: Lunch on Thursday\r\n",
		"Message-ID: <abc123@example.com>\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
		"Content-Transfer-Encoding: quoted-printable\r\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing header %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Bcc:") {
		t.Error("Bcc leaked into the message")
	}
}

func TestBuildBccOnlyWhenAsked(t *testing.T) {
	d := &Draft{
		From: model.Address{Email: "bob@example.com"},
		To:   []model.Address{{Email: "jane@example.org"}},
		Bcc:  []model.Address{{Email: "secret@example.org"}},
	}
	raw, err := Build(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret@example.org") {
		t.Error("Bcc included by default")
	}

	d.IncludeBcc = true
	raw, err = Build(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Bcc: <secret@example.org>") {
		t.Errorf("Bcc missing with IncludeBcc:\n%s", raw)
	}
}

func TestBuildGeneratesMessageIDAndDate(t *testing.T) {
	raw, err := Build(&Draft{From: model.Address{Email: "bob@example.com"}, TextBody: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.MessageID == "" || !strings.HasSuffix(p.MessageID, "@example.com") {
		t.Errorf("generated message-id = %q", p.MessageID)
	}
	if p.Date.IsZero() || time.Since(p.Date) > time.Minute {
		t.Errorf("generated date = %v", p.Date)
	}

	a, _ := Build(&Draft{TextBody: "x"})
	b, _ := Build(&Draft{TextBody: "x"})
	pa, _ := Parse(a)
	pb, _ := Parse(b)
	if pa.MessageID == pb.MessageID {
		t.Error("generated message ids are not unique")
	}
}

func TestBuildEncodesNonASCIIHeaders(t *testing.T) {
	d := &Draft{
		From:     model.Address{Name: "René Müller", Email: "rene@example.de"},
		To:       []model.Address{{Name: "Anneke de Vries", Email: "anneke@example.nl"}},
		Subject:  "Grüße aus München — café",
		TextBody: "Schöne Grüße,\nRené",
	}
	raw, err := Build(d)
	if err != nil {
		t.Fatal(err)
	}
	checkWireFormat(t, raw)
	for _, b := range raw {
		if b > 0x7e {
			t.Fatalf("message is not 7-bit clean: byte %#x\n%s", b, raw)
		}
	}
	if !strings.Contains(string(raw), "=?utf-8?q?") && !strings.Contains(string(raw), "=?UTF-8?q?") {
		t.Errorf("subject not RFC 2047 encoded:\n%s", raw)
	}
}

func TestBuildWithAttachments(t *testing.T) {
	d := &Draft{
		From:     model.Address{Email: "bob@example.com"},
		To:       []model.Address{{Email: "jane@example.org"}},
		Subject:  "Report",
		TextBody: "See attached.",
		Attachments: []DraftAttachment{
			{Filename: "report.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4 hello")},
			{Filename: "jaarrekening été.txt", Data: bytes.Repeat([]byte("x"), 5000)},
		},
	}
	raw, err := Build(d)
	if err != nil {
		t.Fatal(err)
	}
	checkWireFormat(t, raw)

	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.TextBody != "See attached." {
		t.Errorf("body = %q", p.TextBody)
	}
	if len(p.Attachments) != 2 {
		t.Fatalf("attachments = %+v", p.Attachments)
	}
	if p.Attachments[0].Filename != "report.pdf" || p.Attachments[0].ContentType != "application/pdf" {
		t.Errorf("att0 = %+v", p.Attachments[0])
	}
	if p.Attachments[1].Filename != "jaarrekening été.txt" {
		t.Errorf("att1 filename = %q", p.Attachments[1].Filename)
	}
	if p.Attachments[1].ContentType != "application/octet-stream" {
		t.Errorf("att1 type = %q", p.Attachments[1].ContentType)
	}

	data, ct, fn, err := PartContent(raw, p.Attachments[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "%PDF-1.4 hello" || ct != "application/pdf" || fn != "report.pdf" {
		t.Errorf("part content = %q %q %q", data, ct, fn)
	}
	big, _, _, err := PartContent(raw, p.Attachments[1].Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(big) != 5000 {
		t.Errorf("second attachment is %d bytes, want 5000", len(big))
	}
}

func TestBuildRoundTrip(t *testing.T) {
	d := &Draft{
		From:       model.Address{Name: "Bob Smith", Email: "bob@example.com"},
		To:         []model.Address{{Name: "Jane Doe", Email: "jane@example.org"}, {Email: "second@example.org"}},
		Cc:         []model.Address{{Name: "Team, The", Email: "team@example.org"}},
		Subject:    "Re: Grüße — a rather long subject line that will need folding across lines",
		TextBody:   "Line one.\n\nLine two with a very long run of text that quoted-printable has to soft-wrap because it is definitely longer than seventy-six characters.\n\nBob",
		InReplyTo:  "parent@example.org",
		References: []string{"root@example.org", "<parent@example.org>"},
		Date:       time.Date(2025, 2, 3, 9, 14, 0, 0, time.UTC),
		MessageID:  "roundtrip@example.com",
	}
	raw, err := Build(d)
	if err != nil {
		t.Fatal(err)
	}
	checkWireFormat(t, raw)

	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.From != d.From {
		t.Errorf("from = %+v, want %+v", p.From, d.From)
	}
	if len(p.To) != 2 || p.To[0] != d.To[0] || p.To[1] != d.To[1] {
		t.Errorf("to = %+v", p.To)
	}
	if len(p.Cc) != 1 || p.Cc[0] != d.Cc[0] {
		t.Errorf("cc = %+v", p.Cc)
	}
	if p.Subject != d.Subject {
		t.Errorf("subject = %q, want %q", p.Subject, d.Subject)
	}
	if p.MessageID != d.MessageID {
		t.Errorf("message-id = %q", p.MessageID)
	}
	if p.InReplyTo != d.InReplyTo {
		t.Errorf("in-reply-to = %q", p.InReplyTo)
	}
	if len(p.References) != 2 || p.References[0] != "root@example.org" || p.References[1] != "parent@example.org" {
		t.Errorf("references = %v", p.References)
	}
	if !p.Date.Equal(d.Date) {
		t.Errorf("date = %v, want %v", p.Date, d.Date)
	}
	if p.TextBody != normalizeText(d.TextBody) {
		t.Errorf("body =\n%q\nwant\n%q", p.TextBody, normalizeText(d.TextBody))
	}
	if p.HasHTML || len(p.Attachments) != 0 {
		t.Errorf("unexpected html/attachments: %v %+v", p.HasHTML, p.Attachments)
	}
}

func TestBuildNilDraft(t *testing.T) {
	if _, err := Build(nil); err == nil {
		t.Error("Build(nil) = no error")
	}
}

func TestBuildLongUnbreakableBody(t *testing.T) {
	raw, err := Build(&Draft{
		From:     model.Address{Email: "a@b.com"},
		TextBody: strings.Repeat("abcdefghij", 500),
	})
	if err != nil {
		t.Fatal(err)
	}
	checkWireFormat(t, raw)
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.TextBody != strings.Repeat("abcdefghij", 500) {
		t.Errorf("body round trip lost data (%d bytes)", len(p.TextBody))
	}
}

func TestBuildManyRecipientsFold(t *testing.T) {
	var to []model.Address
	for i := 0; i < 40; i++ {
		to = append(to, model.Address{Name: "Recipient Number", Email: string(rune('a'+i%26)) + "somebody@example.com"})
	}
	raw, err := Build(&Draft{From: model.Address{Email: "a@b.com"}, To: to, TextBody: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	checkWireFormat(t, raw)
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.To) != 40 {
		t.Errorf("round-tripped %d recipients, want 40", len(p.To))
	}
}

func TestBuildLongUnfoldableHeaders(t *testing.T) {
	longSubject := strings.Repeat("x", 2000)
	urlSubject := "Re: see https://example.com/" + strings.Repeat("a", 1500)
	for _, tc := range []struct {
		name    string
		draft   *Draft
		subject string
	}{
		{"unbroken subject", &Draft{Subject: longSubject}, longSubject},
		{"long url in subject", &Draft{Subject: urlSubject}, urlSubject},
		{"long non-ascii subject", &Draft{Subject: strings.Repeat("Grüße", 300)}, strings.Repeat("Grüße", 300)},
		{
			"unbreakable reference",
			&Draft{Subject: "s", References: []string{strings.Repeat("r", 1200) + "@example.com"}},
			"s",
		},
		{
			"one very long recipient",
			&Draft{Subject: "s", To: []model.Address{{Email: strings.Repeat("t", 1200) + "@example.com"}}},
			"s",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.draft.From = model.Address{Email: "a@example.com"}
			tc.draft.TextBody = "hi"
			tc.draft.Date = time.Unix(0, 0).UTC()
			raw, err := Build(tc.draft)
			if err != nil {
				t.Fatal(err)
			}
			checkWireFormat(t, raw)
			p, err := Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if p.Subject != tc.subject {
				t.Errorf("subject round-trip:\n got %q\nwant %q", p.Subject, tc.subject)
			}
		})
	}
}

func TestBuildLongBodyLineWraps(t *testing.T) {
	body := strings.Repeat("z", 5000)
	raw, err := Build(&Draft{
		From:     model.Address{Email: "a@example.com"},
		Subject:  "long line",
		TextBody: body,
		Date:     time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	checkWireFormat(t, raw)
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.TextBody != body {
		t.Errorf("5000-byte body line did not round-trip (%d bytes back)", len(p.TextBody))
	}
}
