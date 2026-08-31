package mime

import (
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"

	"github.com/teulaert/emlcalsync/internal/model"
)

// HTMLDocOptions steers HTMLDocument.
type HTMLDocOptions struct {
	// Fetch pulls the pictures the sender hosts elsewhere, to be folded into
	// the page as data: URIs. Nil leaves them out: the page then shows only
	// what travelled inside the message, and nothing about it reaches the
	// network at any point. See HTMLDocument.
	Fetch FetchFunc
	// MaxInlineBytes caps the total size of everything folded into the page
	// as data: URIs, the message's own parts and the fetched pictures
	// together. Zero means defaultMaxInline.
	MaxInlineBytes int
}

// FetchFunc returns one remote asset and the content type to write into its
// data: URI. Every error is ordinary: the reference is left alone and the
// browser shows a broken image. internal/webasset has the implementation,
// and the reasons it is careful.
type FetchFunc func(ctx context.Context, url string) (data []byte, contentType string, err error)

const defaultMaxInline = 24 << 20 // 24 MB of images is already an absurd mail

// contentSecurityPolicy keeps the browser off the network, whatever the page
// turns out to contain. It is unconditional: there is no mode in which the
// page fetches for itself.
//
// A browser asking a tracking host for a pixel sends the cookies it holds for
// that host, which tells the sender which account opened the message rather
// than merely that somebody did; it also hands over its own headers and keeps
// the result in a cache shared with ordinary browsing. When pictures are
// wanted, emlcal fetches them itself (see HTMLDocOptions.Fetch) and folds
// them in below, so what the browser opens is a self-contained file. That
// leaves data: images, data: fonts and inline CSS -- which is how mail is
// styled -- and nothing else, scripts and form posts included.
const contentSecurityPolicy = "default-src 'none'; " +
	"img-src data:; media-src data:; font-src data:; " +
	"style-src 'unsafe-inline'; form-action 'none'"

// HTMLDocument renders one archived message as a standalone HTML page: the
// sender's own markup, its pictures folded in as data: URIs, a short header
// block above it, and a policy that stops the page fetching anything itself.
//
// The pictures a message hosts elsewhere are the ones worth thinking about.
// Left out, a marketing mail collapses -- the tables are sized by the images
// that are no longer there -- and what is left is not worth opening a browser
// for. So opts.Fetch, when set, pulls them here and folds them in, and the
// page stays a file that loads nothing. What that cannot hide is the fetch:
// the URL is minted per recipient, so asking for it tells the sender the
// message was opened. A nil Fetch is the answer for someone who would rather
// it did not, and `remote_content = false` in config.toml is how they say so.
//
// It is the escape hatch for mail the text extractor mangles. HTML mail is
// not a document format so much as a pile of nested tables, and any
// html-to-text pass is a heuristic that will sometimes lose the one line that
// mattered (a one-time login code, say). Rather than chase every such
// message, hand the person the thing the sender actually wrote.
//
// A message with no HTML part is still rendered: its text goes in a <pre>, so
// `mail open` never answers "there is nothing to show".
func HTMLDocument(ctx context.Context, raw []byte, opts HTMLDocOptions) ([]byte, error) {
	p, err := Parse(raw)
	if err != nil {
		return nil, err
	}

	budget := opts.maxInline()
	var body string
	var remote remoteResult
	if p.HasHTML && p.HTMLPart != "" {
		data, _, _, err := PartContent(raw, p.HTMLPart)
		if err != nil {
			return nil, err
		}
		// The message's own parts go first: they are already here, they cost
		// no network, and spending the budget on them beats spending it on
		// something that has to be downloaded.
		body = inlineCIDs(decodeBytes(data), raw, p, &budget)
		if opts.Fetch != nil {
			body, remote = inlineRemote(ctx, body, opts.Fetch, &budget)
		}
	} else {
		body = "<pre style=\"white-space:pre-wrap;font:14px/1.5 ui-monospace,monospace\">" +
			html.EscapeString(p.TextBody) + "</pre>"
	}

	var b strings.Builder
	b.WriteString("<!doctype html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n")
	// First in the head, before anything it has to govern.
	b.WriteString("<meta http-equiv=\"Content-Security-Policy\" content=\"" +
		contentSecurityPolicy + "\">\n")
	b.WriteString("<title>" + html.EscapeString(titleOf(p)) + "</title>\n</head>\n")
	// The sender's <body> attributes are lifted onto ours: a message that
	// paints its own background and writes in a colour to match it would be
	// unreadable without them.
	b.WriteString("<body" + bodyAttrs(body) + ">\n")
	b.WriteString(headerBlock(p, opts.Fetch != nil, remote))
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
	reBodyAttr = regexp.MustCompile(`(?is)\b(style|bgcolor|background|text|dir|lang)\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
)

// bodyAttrs lifts the presentational attributes of the message's own <body>
// onto the page's -- the sender's <body> tag itself is stripped, so without
// this a message that paints its own background would lose it. background=
// is among them because it is how HTML4 mail wallpapers a page, and by the
// time this runs it is a data: URI rather than a URL. Anything else the tag
// carried (class names, ARIA roles, event handlers) is left behind.
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
func inlineCIDs(h string, raw []byte, p *Parsed, bud *int) string {
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
		if err != nil || len(data) == 0 || len(data) > *bud {
			return m
		}
		*bud -= len(data)
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
//
// It also says what was done about the pictures hosted elsewhere. A reader
// looking at a grey rectangle deserves to know whether it is a picture that
// was refused or one that failed, and a reader whose pictures are all there
// deserves to know that asking for them was noticed.
func headerBlock(p *Parsed, fetched bool, r remoteResult) string {
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
	if note := remoteNote(fetched, r); note != "" {
		b.WriteString(`<div style="margin-top:6px;color:#666;font-size:12px">` +
			note + `</div>`)
	}
	b.WriteString("</div>\n")
	return b.String()
}

// remoteNote is the line under the headers about the pictures the sender
// hosts elsewhere. Silence when there were none: a plain-text message or a
// self-contained one has nothing to explain.
func remoteNote(fetched bool, r remoteResult) string {
	switch {
	case !fetched:
		return "Pictures the sender hosts elsewhere were left out, and nothing " +
			"about this message left your machine. --remote fetches them, " +
			"O in the tui."
	case !r.tried():
		return ""
	case r.failed == 0:
		return plural(r.ok, "picture", "pictures") + " came from the sender's " +
			"servers and now travel inside this page, which tells the sender the " +
			"message was opened. The page itself loads nothing."
	case r.ok == 0:
		return plural(r.failed, "picture", "pictures") + " hosted elsewhere could " +
			"not be fetched, and show as broken."
	default:
		return plural(r.ok, "picture", "pictures") + " came from the sender's " +
			"servers, which tells the sender the message was opened; " +
			plural(r.failed, "other", "others") + " could not be fetched. " +
			"The page itself loads nothing."
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
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
