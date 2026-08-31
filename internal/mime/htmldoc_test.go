package mime

import (
	"context"
	"strings"
	"testing"
)

// The message this whole escape hatch exists for: html2text loses the body of
// a message whose <style> tag carries attributes and whose CSS carries a bare
// child combinator, so the one line that mattered never reaches the reader.
// The page has to carry it.
func TestHTMLDocumentKeepsWhatTextExtractionLost(t *testing.T) {
	raw := load(t, "html_style_selectors.eml")

	p := mustParse(t, "html_style_selectors.eml")
	if strings.Contains(p.TextBody, "678863") {
		t.Skip("html2text no longer loses this body; the fixture needs a new one")
	}

	doc, err := HTMLDocument(t.Context(), raw, HTMLDocOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := string(doc)
	for _, want := range []string{
		"678863",
		"Your one-time verification code",
		"noreply@example.com",
		"jane@example.org",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestHTMLDocumentBlocksRemoteContent(t *testing.T) {
	raw := load(t, "html_only.eml")

	doc, err := HTMLDocument(t.Context(), raw, HTMLDocOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := string(doc)
	if !strings.Contains(got, `http-equiv="Content-Security-Policy"`) {
		t.Fatal("no content security policy: opening a message would fire its tracking pixels")
	}
	if !strings.Contains(got, "default-src 'none'") {
		t.Errorf("policy does not deny by default: %q", got[:min(len(got), 400)])
	}
	// The policy has to come before anything it governs.
	if i, j := strings.Index(got, "Content-Security-Policy"), strings.Index(got, "<body"); i > j {
		t.Errorf("policy at %d comes after <body> at %d", i, j)
	}

	// The policy is unconditional: fetching the pictures is emlcal's job, so
	// there is no longer a mode in which the browser is let off the leash.
	withPictures, err := HTMLDocument(t.Context(), raw, HTMLDocOptions{
		Fetch: func(context.Context, string) ([]byte, string, error) {
			return []byte("\x89PNG\r\n\x1a\n"), "image/png", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withPictures), "default-src 'none'") {
		t.Error("fetching the pictures dropped the policy off the page")
	}
}

func TestHTMLDocumentInlinesCIDImages(t *testing.T) {
	raw := load(t, "related_inline.eml")

	doc, err := HTMLDocument(t.Context(), raw, HTMLDocOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := string(doc)
	if strings.Contains(got, "cid:photo1@example.net") {
		t.Error("cid: reference left as it was; the image cannot load under the policy")
	}
	if !strings.Contains(got, "data:image/png;base64,iVBORw0KGgo") {
		t.Error("inline image was not folded into the page")
	}
}

// A cid: nothing answers stays as it was: a broken image says "there was a
// picture here", which is truer than silence.
func TestHTMLDocumentLeavesUnknownCID(t *testing.T) {
	raw := strings.ReplaceAll(string(load(t, "related_inline.eml")),
		`<img src="cid:photo1@example.net">`,
		`<img src="cid:photo1@example.net"><img src="cid:gone@example.net">`)

	doc, err := HTMLDocument(t.Context(), []byte(raw), HTMLDocOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "cid:gone@example.net") {
		t.Error("unknown cid: reference was rewritten to something")
	}
}

func TestHTMLDocumentCapsInlineBytes(t *testing.T) {
	raw := load(t, "related_inline.eml")

	doc, err := HTMLDocument(t.Context(), raw, HTMLDocOptions{MaxInlineBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), "data:image/png") {
		t.Error("an image over the budget was inlined anyway")
	}
	if !strings.Contains(string(doc), "cid:photo1@example.net") {
		t.Error("the reference should be left alone when the image will not fit")
	}
}

// A message that paints its own background and writes in a colour to match it
// would be unreadable if the <body> attributes were dropped on the way in.
func TestHTMLDocumentLiftsBodyAttributes(t *testing.T) {
	raw := load(t, "html_style_selectors.eml")

	doc, err := HTMLDocument(t.Context(), raw, HTMLDocOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := string(doc)
	if !strings.Contains(got, `<body style="background-color:#d9d9d6;">`) {
		t.Errorf("body attributes were not lifted: %q", firstTag(got, "<body"))
	}
	if strings.Count(got, "<body") != 1 {
		t.Errorf("want exactly one <body>, got %d", strings.Count(got, "<body"))
	}
	if strings.Contains(got, "<!doctype html><html") {
		t.Error("the message's own document tags were left nested inside ours")
	}
}

// Text-only mail still renders: `mail open` never answers "nothing to show".
func TestHTMLDocumentFallsBackToText(t *testing.T) {
	raw := []byte("From: a@example.com\r\nTo: b@example.org\r\n" +
		"Subject: Plain\r\n\r\nline one\r\nline <two>\r\n")

	doc, err := HTMLDocument(t.Context(), raw, HTMLDocOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := string(doc)
	if !strings.Contains(got, "<pre") || !strings.Contains(got, "line one") {
		t.Errorf("text body not rendered: %q", got)
	}
	if !strings.Contains(got, "line &lt;two&gt;") {
		t.Error("text body was not escaped on the way into the page")
	}
}

func firstTag(s, open string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], ">")
	if j < 0 {
		return s[i:]
	}
	return s[i : i+j+1]
}

// Outlook writes id="x_cid:<uuid>" beside the real reference. Rewriting that
// attribute would break the element rather than show a picture.
func TestHTMLDocumentIgnoresCIDInsideALongerWord(t *testing.T) {
	raw := strings.ReplaceAll(string(load(t, "related_inline.eml")),
		`<img src="cid:photo1@example.net">`,
		`<span id="x_cid:photo1@example.net">&lt;pixel.png&gt;</span>`+
			`<img src="cid:photo1@example.net">`)

	doc, err := HTMLDocument(t.Context(), []byte(raw), HTMLDocOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := string(doc)
	if !strings.Contains(got, `id="x_cid:photo1@example.net"`) {
		t.Error("the x_cid attribute was rewritten; the element is now broken")
	}
	if !strings.Contains(got, `src="data:image/png;base64,`) {
		t.Error("the real reference beside it was not inlined")
	}
}

// A reference written as text runs straight into the tag after it. The value
// has to stop at the markup, or the lookup fails and the tag is eaten.
func TestHTMLDocumentStopsCIDAtMarkup(t *testing.T) {
	for _, tail := range []string{"</span>", "]<br>", ")"} {
		raw := strings.ReplaceAll(string(load(t, "related_inline.eml")),
			`<img src="cid:photo1@example.net">`,
			`<span>cid:photo1@example.net`+tail)

		doc, err := HTMLDocument(t.Context(), []byte(raw), HTMLDocOptions{})
		if err != nil {
			t.Fatal(err)
		}
		got := string(doc)
		if !strings.Contains(got, "data:image/png;base64,") {
			t.Errorf("tail %q: reference was not resolved", tail)
		}
		if !strings.Contains(got, strings.TrimSuffix(tail, ")")) {
			t.Errorf("tail %q: the markup after the reference was swallowed", tail)
		}
	}
}
