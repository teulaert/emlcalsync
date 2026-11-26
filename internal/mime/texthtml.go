package mime

import (
	"html"
	"regexp"
	"strings"
)

// TextToHTML renders a plain-text body as the minimal HTML that goes next to
// it in a multipart/alternative.
//
// It exists for Apple Mail and Outlook, not for looks. Both lay a message that
// is only text/plain out in a fixed, narrow reading column however wide the
// window is, and a recipient takes that for a mail that was wrapped by hand
// (EML-4). Every mainstream client sends an HTML alternative, and with one the
// text flows to the width of the window as it does in Gmail.
//
// So the HTML says exactly what the text says and nothing more: no fonts, no
// colours, no styles, so the reader's defaults apply. Paragraphs are <p>, a
// single line break is <br>, runs of spaces survive as &nbsp;, http(s) URLs
// are links, and lines quoted with "> " become nested <blockquote
// type="cite">, which is what clients draw as a quote bar. <p> and <br> rather
// than CSS white-space because Outlook's Word renderer ignores CSS.
func TextToHTML(text string) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\"></head><body>\n")

	depth := 0 // blockquotes currently open
	var para []string
	flush := func() {
		if len(para) == 0 {
			return
		}
		b.WriteString("<p>")
		for i, l := range para {
			if i > 0 {
				b.WriteString("<br>\n")
			}
			b.WriteString(lineToHTML(l))
		}
		b.WriteString("</p>\n")
		para = para[:0]
	}

	for _, line := range strings.Split(strings.TrimRight(normalizeText(text), "\n"), "\n") {
		d, rest := quoteDepth(line)
		if d != depth {
			flush()
			for ; depth < d; depth++ {
				b.WriteString("<blockquote type=\"cite\">\n")
			}
			for ; depth > d; depth-- {
				b.WriteString("</blockquote>\n")
			}
		}
		if strings.TrimSpace(rest) == "" {
			flush()
			continue
		}
		para = append(para, rest)
	}
	flush()
	for ; depth > 0; depth-- {
		b.WriteString("</blockquote>\n")
	}
	b.WriteString("</body></html>\n")
	return b.String()
}

// quoteDepth counts the leading ">" quote markers of a line (with or without a
// space between them, as clients write "> > " and ">> " alike) and returns the
// line without them and without the one space after the last.
func quoteDepth(line string) (int, string) {
	d := 0
	rest := line
	for {
		t := strings.TrimLeft(rest, " ")
		if !strings.HasPrefix(t, ">") || (d == 0 && len(t) != len(rest)) {
			break
		}
		d++
		rest = t[1:]
	}
	if d > 0 {
		rest = strings.TrimPrefix(rest, " ")
	}
	return d, rest
}

// reURL finds http(s) links in text. Trailing punctuation that ends a sentence
// rather than the URL is trimmed off by trimURL.
var reURL = regexp.MustCompile(`https?://[^\s<>"]+`)

// lineToHTML escapes one line, links its URLs and keeps its runs of spaces.
func lineToHTML(line string) string {
	var b strings.Builder
	last := 0
	for _, m := range reURL.FindAllStringIndex(line, -1) {
		url := trimURL(line[m[0]:m[1]])
		end := m[0] + len(url)
		b.WriteString(spaces(html.EscapeString(line[last:m[0]])))
		esc := html.EscapeString(url)
		b.WriteString(`<a href="` + esc + `">` + esc + `</a>`)
		last = end
	}
	b.WriteString(spaces(html.EscapeString(line[last:])))
	return b.String()
}

// trimURL drops sentence punctuation from the end of a matched URL, and a
// closing parenthesis that has no opening one inside the URL.
func trimURL(u string) string {
	for len(u) > 0 {
		c := u[len(u)-1]
		switch {
		case strings.IndexByte(".,;:!?'", c) >= 0:
			u = u[:len(u)-1]
		case c == ')' && strings.Count(u, "(") < strings.Count(u, ")"):
			u = u[:len(u)-1]
		default:
			return u
		}
	}
	return u
}

// spaces keeps what HTML would collapse: a tab becomes four spaces, and in a
// run of spaces every other one (and a leading one) is a non-breaking space,
// so the run keeps its width while the line can still wrap.
func spaces(s string) string {
	s = strings.ReplaceAll(s, "\t", "    ")
	if !strings.Contains(s, "  ") && !strings.HasPrefix(s, " ") {
		return s
	}
	var b strings.Builder
	prevSpace := true // a space at the start of a line would collapse too
	for _, r := range s {
		if r == ' ' {
			if prevSpace {
				b.WriteString("&nbsp;")
				prevSpace = false
				continue
			}
			prevSpace = true
			b.WriteByte(' ')
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return b.String()
}
