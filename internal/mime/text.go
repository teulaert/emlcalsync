package mime

import (
	"io"
	stdmime "mime"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message/charset"
	"github.com/k3a/html2text"
	"golang.org/x/text/encoding/charmap"
)

// decodeBytes turns part bytes into a UTF-8 string. go-message has already
// applied any charset it recognised; whatever is left that is not valid UTF-8
// is assumed to be windows-1252 (a superset of latin-1), which is what real
// mail almost always turns out to be.
func decodeBytes(b []byte) string {
	if utf8.Valid(b) {
		return strings.TrimPrefix(string(b), "\ufeff")
	}
	s, err := charmap.Windows1252.NewDecoder().Bytes(b)
	if err != nil {
		// The decoder substitutes unassigned bytes, so this is practically
		// unreachable; fall back to latin-1 rather than return invalid UTF-8.
		var sb strings.Builder
		sb.Grow(len(b))
		for _, c := range b {
			sb.WriteRune(rune(c))
		}
		return sb.String()
	}
	return strings.TrimPrefix(string(s), "\ufeff")
}

var (
	reCRLF       = strings.NewReplacer("\r\n", "\n", "\r", "\n")
	reTrailWS    = regexp.MustCompile(`[ \t\x0b\f]+\n`)
	reManyBlanks = regexp.MustCompile(`\n{4,}`)
	reNul        = strings.NewReplacer("\x00", "")
)

// normalizeText makes body text safe and tidy for storage and display:
// LF line endings, no trailing whitespace on a line, at most two blank lines
// in a row, no leading/trailing blank lines.
func normalizeText(s string) string {
	s = reCRLF.Replace(s)
	s = reNul.Replace(s)
	s = strings.TrimPrefix(s, "\ufeff")
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	s = reTrailWS.ReplaceAllString(s, "\n")
	s = reManyBlanks.ReplaceAllString(s, "\n\n\n")
	return strings.Trim(s, "\n")
}

// Snippet returns the first n runes of text with all whitespace collapsed to
// single spaces, with a horizontal ellipsis appended when text was truncated.
func Snippet(text string, n int) string {
	if n <= 0 {
		return ""
	}
	collapsed := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(collapsed) <= n {
		return collapsed
	}
	i, count := 0, 0
	for i = range collapsed {
		if count == n {
			break
		}
		count++
	}
	// i is the byte offset of rune n (loop breaks before consuming it) unless
	// the string ended, which the length check above already excluded.
	return strings.TrimRight(collapsed[:i], " ") + "…"
}

// ---------------------------------------------------------------------------
// HTML

var (
	reComment   = regexp.MustCompile(`(?is)<!--.*?-->`)
	reBlockEnd  = regexp.MustCompile(`(?i)</(div|tr|table|thead|tbody|blockquote|section|article|header|footer|pre|form|dd|dt|caption)\s*>`)
	reBlockOpen = regexp.MustCompile(`(?i)<(hr)(\s[^>]*)?/?>`)
	reCellEnd   = regexp.MustCompile(`(?i)</(td|th)\s*>`)
	reLinkTail  = regexp.MustCompile(`\s<((?:https?|mailto|ftp)://?[^\s<>]+)>`)
	reHTMLSpace = regexp.MustCompile(`[ \t]+`)
)

// htmlToText converts an HTML body to readable plain text: link targets are
// kept as "text (url)", table rows stay on their own lines, script/style/head
// content is dropped.
func htmlToText(h string) string {
	h = reComment.ReplaceAllString(h, "")
	// html2text only breaks lines on <br>/<p>/<li>/<h*>; mail is mostly built
	// from divs and tables, so give it explicit breaks for those.
	h = reBlockEnd.ReplaceAllString(h, "<br>$0")
	h = reBlockOpen.ReplaceAllString(h, "<br>$0")
	h = reCellEnd.ReplaceAllString(h, " $0")

	out := html2text.HTML2TextWithOptions(h,
		html2text.WithUnixLineBreaks(),
		html2text.WithLinksInnerText(),
		html2text.WithListSupport(),
	)

	// html2text renders <a> as `text <url>`; the house style is `text (url)`.
	out = reLinkTail.ReplaceAllStringFunc(out, func(m string) string {
		url := strings.TrimSuffix(strings.TrimPrefix(strings.TrimLeft(m, " \t\n"), "<"), ">")
		return " (" + url + ")"
	})
	// Drop a duplicated target: "https://x (https://x)" -> "https://x".
	out = dedupLinkText(out)

	lines := strings.Split(reCRLF.Replace(out), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(reHTMLSpace.ReplaceAllString(l, " "))
	}
	return normalizeText(strings.Join(lines, "\n"))
}

var reDupLink = regexp.MustCompile(`(\S+) \((\S+)\)`)

func dedupLinkText(s string) string {
	return reDupLink.ReplaceAllStringFunc(s, func(m string) string {
		g := reDupLink.FindStringSubmatch(m)
		if g[1] == g[2] {
			return g[1]
		}
		return m
	})
}

// ---------------------------------------------------------------------------
// RFC 2047

var wordDecoder = &stdmime.WordDecoder{CharsetReader: func(cs string, input io.Reader) (io.Reader, error) {
	return charset.Reader(cs, input)
}}

// decodeWord decodes RFC 2047 encoded words, tolerating malformed input by
// returning the original text.
func decodeWord(s string) string {
	if s == "" {
		return s
	}
	if dec, err := wordDecoder.DecodeHeader(s); err == nil {
		// A partially encoded header can leave raw 8-bit bytes behind next to
		// correctly decoded words; repair those without mangling the rest.
		return unfold(repairUTF8(dec))
	}
	return unfold(decodeBytes([]byte(s)))
}

func unfold(s string) string {
	s = reCRLF.Replace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(reHTMLSpace.ReplaceAllString(s, " "))
}

// repairUTF8 replaces bytes that are not part of a valid UTF-8 sequence with
// their windows-1252 meaning, leaving valid sequences untouched.
func repairUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			sb.WriteRune(charmap.Windows1252.DecodeByte(s[i]))
			i++
			continue
		}
		sb.WriteString(s[i : i+size])
		i += size
	}
	return sb.String()
}
