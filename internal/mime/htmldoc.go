package mime

import (
	"encoding/base64"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/teulaert/emlcalsync/internal/model"
)

// HTMLDocOptions steers HTMLDocument.
type HTMLDocOptions struct {
	// AllowRemote leaves the message free to fetch from the network. Off by
	// default, and deliberately so -- see HTMLDocument.
	AllowRemote bool
	// MaxInlineBytes caps the total size of the inline parts folded into the
	// page as data: URIs. Zero means defaultMaxInline.
	MaxInlineBytes int
}

const defaultMaxInline = 24 << 20 // 24 MB of images is already an absurd mail

// contentSecurityPolicy is what keeps opening a message from telling its
// sender that it was opened.
//
// HTML mail is full of one-pixel images on a tracking URL, and a browser is
// exactly the thing that will fetch them -- along with web fonts, remote
// stylesheets and whatever else the sender put in. An archive whose reads
// otherwise never touch the network should not start doing so because the
// text extractor gave up. So the page declares that it may load nothing:
// data: images (the message's own inline parts, folded in below) and inline
// CSS (which is how mail is styled) are all that is left.
const contentSecurityPolicy = "default-src 'none'; " +
	"img-src data:; media-src data:; font-src data:; " +
	"style-src 'unsafe-inline'; form-action 'none'"

// HTMLDocument renders one archived message as a standalone HTML page: the
// sender's own markup, its inline images folded in as data: URIs, a short
// header block above it, and -- unless AllowRemote is set -- a policy that
// blocks every fetch to the network.
//
// It is the escape hatch for mail the text extractor mangles. HTML mail is
// not a document format so much as a pile of nested tables, and any
// html-to-text pass is a heuristic that will sometimes lose the one line that
// mattered (a one-time login code, say). Rather than chase every such
// message, hand the person the thing the sender actually wrote.
//
// A message with no HTML part is still rendered: its text goes in a <pre>, so
// `mail open` never answers "there is nothing to show".
func HTMLDocument(raw []byte, opts HTMLDocOptions) ([]byte, error) {
	p, err := Parse(raw)
	if err != nil {
		return nil, err
	}

	var body string
	if p.HasHTML && p.HTMLPart != "" {
		data, _, _, err := PartContent(raw, p.HTMLPart)
		if err != nil {
			return nil, err
		}
		body = inlineCIDs(decodeBytes(data), raw, p, opts.maxInline())
	} else {
		body = "<pre style=\"white-space:pre-wrap;font:14px/1.5 ui-monospace,monospace\">" +
			html.EscapeString(p.TextBody) + "</pre>"
	}

	var b strings.Builder
	b.WriteString("<!doctype html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n")
	if !opts.AllowRemote {
		// First in the head, before anything it has to govern.
		b.WriteString("<meta http-equiv=\"Content-Security-Policy\" content=\"" +
			contentSecurityPolicy + "\">\n")
	}
	b.WriteString("<title>" + html.EscapeString(titleOf(p)) + "</title>\n</head>\n")
	// The sender's <body> attributes are lifted onto ours: a message that
	// paints its own background and writes in a colour to match it would be
	// unreadable without them.
	b.WriteString("<body" + bodyAttrs(body) + ">\n")
	b.WriteString(headerBlock(p, opts.AllowRemote))
	b.WriteString(stripDocumentTags(body))
	b.WriteString("\n</body>\n</html>\n")
	return []byte(b.String()), nil
}

func (o HTMLDocOptions) maxInline() int {
	if o.MaxInlineBytes > 0 {
		return o.MaxInlineBytes
	}
	return defaultMaxInline
}

func titleOf(p *Parsed) string {
	if s := strings.TrimSpace(p.Subject); s != "" {
		return s
	}
	return "(no subject)"
}

var (
	reDoctype  = regexp.MustCompile(`(?is)^\s*<!doctype[^>]*>`)
	reHTMLOpen = regexp.MustCompile(`(?is)</?html[^>]*>`)
	reHeadTag  = regexp.MustCompile(`(?is)</?head[^>]*>`)
	reBodyOpen = regexp.MustCompile(`(?is)<body([^>]*)>`)
	reBodyTag  = regexp.MustCompile(`(?is)</?body[^>]*>`)
	reBodyAttr = regexp.MustCompile(`(?is)\b(style|bgcolor|text|dir|lang)\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
)

// bodyAttrs lifts the presentational attributes of the message's own <body>
// onto the page's. Anything else it carried (class names, ARIA roles, event
// handlers) is left behind.
func bodyAttrs(h string) string {
	m := reBodyOpen.FindStringSubmatch(h)
	if m == nil {
		return ""
	}
	var out strings.Builder
	for _, a := range reBodyAttr.FindAllString(m[1], -1) {
		out.WriteString(" " + a)
	}
	return out.String()
}

// stripDocumentTags removes the tags that only make sense once per page.
// The message's markup is nested inside ours, and a browser would drop these
// anyway -- doing it here keeps what it does predictable, and keeps a
// <head> of the sender's from swallowing the block above it.
func stripDocumentTags(h string) string {
	h = reDoctype.ReplaceAllString(h, "")
	h = reHTMLOpen.ReplaceAllString(h, "")
	h = reHeadTag.ReplaceAllString(h, "")
	h = reBodyTag.ReplaceAllString(h, "")
	return strings.TrimSpace(h)
}

// reCID matches a cid: reference. The leading group keeps it from firing
// inside a longer word -- Outlook writes id="x_cid:<uuid>" next to the real
// reference, and rewriting that attribute would break the element rather than
// show a picture. The value stops at markup, so a reference written as text
// ("[cid:image001.png@01D…]</span>") does not swallow the tag after it.
var reCID = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.\-])cid:([^"'\s<>)\]]+)`)

// inlineCIDs replaces cid: references with data: URIs built from the parts
// they name, so the page carries its own images and needs no network for
// them. A reference nothing answers is left as it is: a broken image says
// "there was a picture here", which is truer than silence.
func inlineCIDs(h string, raw []byte, p *Parsed, budget int) string {
	if !strings.Contains(strings.ToLower(h), "cid:") {
		return h
	}
	byID := map[string]Part{}
	for _, part := range p.AllParts {
		if id := strings.Trim(part.ContentID, "<>"); id != "" {
			byID[id] = part
		}
	}
	if len(byID) == 0 {
		return h
	}
	cache := map[string]string{}
	return reCID.ReplaceAllStringFunc(h, func(m string) string {
		g := reCID.FindStringSubmatch(m)
		lead, ref := g[1], g[2]
		if uri, ok := cache[ref]; ok {
			return lead + uri
		}
		part, ok := byID[ref]
		if !ok {
			return m
		}
		data, ctype, _, err := PartContent(raw, part.Path)
		if err != nil || len(data) == 0 || len(data) > budget {
			return m
		}
		budget -= len(data)
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		uri := "data:" + ctype + ";base64," + base64.StdEncoding.EncodeToString(data)
		cache[ref] = uri
		return lead + uri
	})
}

// headerBlock is the strip above the message: who it is from, who it went to,
// when, and what it is about. It uses inline styles on a single element so
// the sender's own CSS has nothing to select it by.
func headerBlock(p *Parsed, allowRemote bool) string {
	const wrap = `<div style="font:13px/1.6 ui-sans-serif,system-ui,sans-serif;color:#111;` +
		`background:#fff;border-bottom:1px solid #ccc;padding:12px 16px;margin:0 0 12px">`
	var b strings.Builder
	b.WriteString(wrap)
	row := func(label, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		b.WriteString(`<div><span style="display:inline-block;min-width:4.5em;color:#666">` +
			html.EscapeString(label) + `</span>` + html.EscapeString(value) + `</div>`)
	}
	b.WriteString(`<div style="font-weight:600;font-size:15px;margin-bottom:4px">` +
		html.EscapeString(titleOf(p)) + `</div>`)
	row("From", addrLine([]model.Address{p.From}))
	row("To", addrLine(p.To))
	row("Cc", addrLine(p.Cc))
	if !p.Date.IsZero() {
		row("Date", p.Date.Local().Format("Mon 2 Jan 2006 15:04 MST"))
	}
	if !allowRemote {
		b.WriteString(`<div style="margin-top:6px;color:#666;font-size:12px">` +
			`Remote content is blocked. Images and styles the sender hosts elsewhere ` +
			`will not load, and nothing on this page reaches the network.</div>`)
	}
	b.WriteString("</div>\n")
	return b.String()
}

func addrLine(as []model.Address) string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		switch {
		case a.Name != "" && a.Email != "":
			out = append(out, fmt.Sprintf("%s <%s>", a.Name, a.Email))
		case a.Email != "":
			out = append(out, a.Email)
		case a.Name != "":
			out = append(out, a.Name)
		}
	}
	return strings.Join(out, ", ")
}
