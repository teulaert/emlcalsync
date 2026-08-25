package mime

import (
	"strings"
	"testing"
)

func TestStripQuotes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain quoted lines",
			in: "Sure, that works for me.\n" +
				"\n" +
				"> Can you make Thursday?\n" +
				"> > Original question here\n",
			want: "Sure, that works for me.",
		},
		{
			name: "english gmail attribution",
			in: "Yes, Thursday works.\n" +
				"\n" +
				"On Mon, 3 Feb 2025 at 09:14, Jane Doe <jane@example.com> wrote:\n" +
				"\n" +
				"> Can you make Thursday?\n" +
				"> Jane\n",
			want: "Yes, Thursday works.",
		},
		{
			name: "english attribution wrapped over two lines",
			in: "Looks good to me.\n" +
				"\n" +
				"On Mon, Feb 3, 2025 at 9:14 AM Jane Doe <jane@example.com>\n" +
				"wrote:\n" +
				"\n" +
				"Original text that was not even quoted.\n",
			want: "Looks good to me.",
		},
		{
			name: "dutch attribution",
			in: "Dank je, dat is prima.\n" +
				"\n" +
				"Op ma 3 feb 2025 om 09:14 schreef Jan Jansen <jan@example.nl>:\n" +
				"\n" +
				"> Kun je donderdag?\n",
			want: "Dank je, dat is prima.",
		},
		{
			name: "dutch outlook header block",
			in: "Zie mijn antwoord hieronder.\n" +
				"\n" +
				"Van: Jan Jansen <jan@example.nl>\n" +
				"Verzonden: maandag 3 februari 2025 09:14\n" +
				"Aan: Anneke de Vries <anneke@example.nl>\n" +
				"Onderwerp: Vergadering\n" +
				"\n" +
				"Kun je donderdag?\n",
			want: "Zie mijn antwoord hieronder.",
		},
		{
			name: "german attribution",
			in: "Passt, bis Donnerstag.\n" +
				"\n" +
				"Am 03.02.2025 um 09:14 schrieb Hans Müller <hans@example.de>:\n" +
				"\n" +
				"> Geht Donnerstag bei dir?\n",
			want: "Passt, bis Donnerstag.",
		},
		{
			name: "french attribution",
			in: "Oui, jeudi me convient.\n" +
				"\n" +
				"Le lun. 3 févr. 2025 à 09:14, Marie Curie <marie@example.fr> a écrit :\n" +
				"\n" +
				"> Es-tu disponible jeudi ?\n",
			want: "Oui, jeudi me convient.",
		},
		{
			name: "outlook original message separator",
			in: "See below.\n" +
				"\n" +
				"-----Original Message-----\n" +
				"From: Jane Doe <jane@example.com>\n" +
				"Sent: Monday, February 3, 2025 9:14 AM\n" +
				"To: Bob <bob@example.com>\n" +
				"Subject: Meeting\n" +
				"\n" +
				"Can you make Thursday?\n",
			want: "See below.",
		},
		{
			name: "outlook english header block without separator",
			in: "Answer inline.\n" +
				"\n" +
				"From: Jane Doe <jane@example.com>\n" +
				"Sent: Monday, February 3, 2025 9:14 AM\n" +
				"To: Bob <bob@example.com>\n" +
				"Cc: Team <team@example.com>\n" +
				"Subject: Meeting\n" +
				"\n" +
				"Can you make Thursday?\n",
			want: "Answer inline.",
		},
		{
			name: "underscore separator before From",
			in: "Short answer: yes.\n" +
				"\n" +
				"________________________________\n" +
				"From: Jane Doe <jane@example.com>\n" +
				"Sent: Monday, February 3, 2025 9:14 AM\n" +
				"To: Bob <bob@example.com>\n" +
				"Subject: Meeting\n" +
				"\n" +
				"Can you make Thursday?\n",
			want: "Short answer: yes.",
		},
		{
			name: "signature delimiter",
			in: "Here is the file.\n" +
				"\n" +
				"-- \n" +
				"Bob Smith\n" +
				"Example BV | +31 6 1234 5678\n",
			want: "Here is the file.",
		},
		{
			name: "signature delimiter without trailing space",
			in: "Here is the file.\n" +
				"\n" +
				"--\n" +
				"Bob Smith\n",
			want: "Here is the file.",
		},
		{
			name: "mobile footer",
			in: "On my way.\n" +
				"\n" +
				"Sent from my iPhone\n",
			want: "On my way.",
		},
		{
			name: "dutch mobile footer",
			in: "Ik ben onderweg.\n" +
				"\n" +
				"Verstuurd vanaf mijn iPhone\n",
			want: "Ik ben onderweg.",
		},
		{
			name: "german mobile footer",
			in: "Bin unterwegs.\n" +
				"\n" +
				"Von meinem iPhone gesendet\n",
			want: "Bin unterwegs.",
		},
		{
			name: "french mobile footer",
			in: "Je suis en route.\n" +
				"\n" +
				"Envoyé de mon iPhone\n",
			want: "Je suis en route.",
		},
		{
			name: "outlook get app footer",
			in: "Works for me.\n" +
				"\n" +
				"Get Outlook for iOS\n",
			want: "Works for me.",
		},
		{
			name: "quotes signature and footer together",
			in: "Thanks, that helps a lot.\n" +
				"\n" +
				"-- \n" +
				"Bob\n" +
				"\n" +
				"On Mon, 3 Feb 2025 at 09:14, Jane Doe <jane@example.com> wrote:\n" +
				"> details\n",
			want: "Thanks, that helps a lot.",
		},
		{
			name: "forwarded message",
			in: "FYI.\n" +
				"\n" +
				"---------- Forwarded message ---------\n" +
				"From: Jane Doe <jane@example.com>\n" +
				"Date: Mon, 3 Feb 2025 at 09:14\n" +
				"Subject: Meeting\n" +
				"\n" +
				"Can you make Thursday?\n",
			want: "FYI.",
		},
		{
			name: "apple begin forwarded message",
			in: "Passing this on.\n" +
				"\n" +
				"Begin forwarded message:\n" +
				"\n" +
				"From: Jane Doe <jane@example.com>\n",
			want: "Passing this on.",
		},
		{
			name: "nothing to strip",
			in:   "Just a plain message.\nWith two lines.\n",
			want: "Just a plain message.\nWith two lines.",
		},
		{
			name: "never returns empty: only a quote",
			in:   "> everything is quoted\n> nothing of my own\n",
			want: "> everything is quoted\n> nothing of my own",
		},
		{
			name: "never returns empty: only an attribution",
			in:   "On Mon, 3 Feb 2025 at 09:14, Jane Doe <jane@example.com> wrote:\n> hi\n",
			want: "On Mon, 3 Feb 2025 at 09:14, Jane Doe <jane@example.com> wrote:\n> hi",
		},
		{
			name: "never returns empty: only a signature",
			in:   "-- \nBob Smith\n",
			want: "--\nBob Smith", // trailing whitespace is normalised away
		},
		{
			name: "empty input",
			in:   "   \n\n",
			want: "",
		},
		{
			name: "prose starting with On is kept",
			in:   "On second thought, let us keep it as it is.\nThat is all.\n",
			want: "On second thought, let us keep it as it is.\nThat is all.",
		},
		{
			name: "double dash inside a sentence is kept",
			in:   "Use the --force flag.\nIt works.\n",
			want: "Use the --force flag.\nIt works.",
		},
		{
			name: "crlf input",
			in:   "Yes.\r\n\r\n> quoted\r\n",
			want: "Yes.",
		},
		{
			name: "blank lines collapsed",
			in:   "One.\n\n\n\n\nTwo.\n\n> quoted\n",
			want: "One.\n\n\nTwo.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := StripQuotes(tc.in)
			if got != tc.want {
				t.Errorf("StripQuotes:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestStripQuotesNeverEmptiesNonEmpty(t *testing.T) {
	inputs := []string{
		"> a", "-- ", "--", "On x wrote:", "Sent from my iPhone",
		"-----Original Message-----", "________________________________",
		"From: a\nSent: b\nTo: c\nSubject: d",
	}
	for _, in := range inputs {
		if got := StripQuotes(in); strings.TrimSpace(got) == "" {
			t.Errorf("StripQuotes(%q) emptied the message", in)
		}
	}
}

func TestStripQuotesIsPure(t *testing.T) {
	in := "Hello.\n\n> quoted\n"
	first := StripQuotes(in)
	second := StripQuotes(in)
	if first != second {
		t.Errorf("not deterministic: %q vs %q", first, second)
	}
	if StripQuotes(first) != first {
		t.Errorf("not idempotent: %q -> %q", first, StripQuotes(first))
	}
}

// TestStripQuotesKeepsSenderText covers the shapes that used to be mistaken for
// quoted material: prose that ends in "wrote:", a quote block with the sender's
// own text after it, and a bare "--" used as a rule.
func TestStripQuotesKeepsSenderText(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{
			name: "sentence ending in wrote: followed by prose",
			in:   "On the subject of the contract, here is what their lawyer wrote:\n\nWe cannot accept clause 4.\n\nPlease read it before Friday.\n",
			want: "On the subject of the contract, here is what their lawyer wrote:\n\nWe cannot accept clause 4.\n\nPlease read it before Friday.",
		},
		{
			name: "repl transcript in the middle",
			in:   "Repro:\n\n>>> import emlcal\n>>> emlcal.sync()\nboom\n\nAny ideas?\n",
			want: "Repro:\n\n>>> import emlcal\n>>> emlcal.sync()\nboom\n\nAny ideas?",
		},
		{
			name: "block quote the sender typed",
			in:   "The spec says:\n\n> messages MUST be idempotent\n\nbut our code is not.\n",
			want: "The spec says:\n\n> messages MUST be idempotent\n\nbut our code is not.",
		},
		{
			name: "bare dash rule with the message continuing",
			in:   "Options:\n\n--\nA) do nothing\nB) rewrite it\n\nWhich one?\n",
			want: "Options:\n\n--\nA) do nothing\nB) rewrite it\n\nWhich one?",
		},
		{
			name: "attribution without a date is still cut when a quote follows",
			in:   "Sure.\n\nOn Friday, Jane wrote:\n> the original\n",
			want: "Sure.",
		},
		{
			name: "quoted block before a mobile footer",
			in:   "Yes.\n\n> quoted\n\nSent from my iPhone\n",
			want: "Yes.",
		},
		{
			name: "mobile footer above the quoted block",
			in:   "Yes.\n\nSent from my iPhone\n\n> quoted\n",
			want: "Yes.",
		},
		{
			name: "em dash signature",
			in:   "Here you go.\n\n—\nBob\n",
			want: "Here you go.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripQuotes(tc.in); got != tc.want {
				t.Errorf("StripQuotes:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}
