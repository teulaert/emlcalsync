package mime

import (
	"strings"
	"testing"
)

func TestUnflow(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		delsp bool
		want  string
	}{
		{
			name: "soft breaks join, hard breaks stay",
			in:   "This is a long \r\nparagraph that \r\nwas wrapped.\r\nNext line.\r\n\r\nNew paragraph.\r\n",
			want: "This is a long paragraph that was wrapped.\nNext line.\n\nNew paragraph.\n",
		},
		{
			name:  "delsp=yes drops the wrapper's space",
			in:    "Donau\r\ndampf \r\nschiff\r\n",
			delsp: true,
			want:  "Donau\ndampfschiff\n",
		},
		{
			name: "space-stuffing is removed",
			in:   " From the top \r\nof the hill\r\n >not a quote\r\n",
			want: "From the top of the hill\n>not a quote\n",
		},
		{
			name: "quoted paragraphs join at their own depth",
			in:   "Sure.\r\n> they said this \r\n> and more\r\n>> older text \r\n>> continues\r\n",
			want: "Sure.\n> they said this and more\n> > older text continues\n",
		},
		{
			name: "a depth change ends a paragraph even after a soft break",
			in:   "> quoted soft \r\nnot quoted\r\n",
			want: "> quoted soft\nnot quoted\n",
		},
		{
			name: "the signature separator is not a soft break",
			in:   "Bye\r\n-- \r\nJan\r\n",
			want: "Bye\n-- \nJan\n",
		},
		{
			name: "a soft line before the signature separator ends there",
			in:   "Regards \r\n-- \r\nAlice\r\n",
			want: "Regards\n-- \nAlice\n",
		},
		{
			name: "the same inside a quote",
			in:   "> Regards \r\n> -- \r\n> Alice\r\n",
			want: "> Regards\n> -- \n> Alice\n",
		},
		{
			name: "empty quoted line",
			in:   "> a\r\n>\r\n> b\r\n",
			want: "> a\n>\n> b\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unflow(tc.in, tc.delsp); got != tc.want {
				t.Errorf("unflow =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

const flowedMail = "From: a@example.com\r\nTo: b@example.com\r\nSubject: flowed\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8; format=flowed; delsp=no\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
	"This paragraph was wrapped by Apple Mail in plain-text mode at about =\r\n" +
	"seventy=20\r\n" +
	"columns, with caf=C3=A9 in it.=20\r\n" +
	"End.\r\n"

func TestParseUnwrapsFormatFlowed(t *testing.T) {
	p, err := Parse([]byte(flowedMail))
	if err != nil {
		t.Fatal(err)
	}
	want := "This paragraph was wrapped by Apple Mail in plain-text mode at about seventy columns, with café in it. End."
	if p.TextBody != want {
		t.Errorf("body =\n%q\nwant\n%q", p.TextBody, want)
	}
}

// The same bytes without format=flowed are taken literally: a trailing space
// means nothing in ordinary text/plain.
func TestParseLeavesPlainTextAlone(t *testing.T) {
	p, err := Parse([]byte(strings.Replace(flowedMail, "; format=flowed; delsp=no", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.TextBody, "seventy\ncolumns") {
		t.Errorf("plain text was rewrapped:\n%q", p.TextBody)
	}
}

// Inside a multipart/alternative, the flowed text part is unwrapped too.
func TestParseUnwrapsFlowedInsideAlternative(t *testing.T) {
	raw := "From: a@example.com\r\nSubject: alt\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=XX\r\n\r\n" +
		"--XX\r\nContent-Type: text/plain; format=\"Flowed\"\r\n\r\none \r\ntwo\r\n" +
		"--XX\r\nContent-Type: text/html\r\n\r\n<p>one two</p>\r\n--XX--\r\n"
	p, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if p.TextBody != "one two" {
		t.Errorf("body = %q, want %q", p.TextBody, "one two")
	}
}
