package mime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	netmail "net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // registers charset decoders
	gomail "github.com/emersion/go-message/mail"

	"github.com/teulaert/emlcalsync/internal/model"
)

// Limits that keep a hostile or corrupt message from exhausting memory.
const (
	maxParts     = 512
	maxDepth     = 24
	maxPartBytes = 64 << 20 // per leaf
	maxTextBytes = 8 << 20  // retained per text leaf
	// maxHeaderPeek is how much of an attached message is read to find its
	// Subject; maxAttachedNameLen caps the filename derived from it.
	maxHeaderPeek      = 64 << 10
	maxAttachedNameLen = 200
)

// ErrEmpty is returned by Parse when raw contains no message at all.
var ErrEmpty = errors.New("mime: empty message")

// leaf is an internal record of one MIME leaf plus its retained content.
type leaf struct {
	part      Part
	mediaType string
	method    string // the Content-Type method parameter of a calendar part
	text      []byte // retained for text/* leaves only
}

// Parse walks raw and extracts headers, body text and part metadata. It is
// deliberately forgiving: unknown charsets, unknown transfer encodings,
// missing MIME-Version headers and broken multipart boundaries all yield a
// best-effort result. An error is returned only when raw holds no message.
func Parse(raw []byte) (p *Parsed, err error) {
	defer func() {
		if r := recover(); r != nil {
			p, err = fallbackParse(raw), nil
		}
	}()

	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, ErrEmpty
	}

	ent, rerr := message.Read(bytes.NewReader(raw))
	if ent == nil {
		if rerr != nil {
			// Not a header block at all: treat the whole thing as text.
			return fallbackParse(raw), nil
		}
		return nil, ErrEmpty
	}

	out := &Parsed{}
	readHeaders(&ent.Header, out)

	w := &walker{}
	w.walk(ent, "", 0)
	if len(w.leaves) == 0 {
		w.leaves = append(w.leaves, leaf{
			part:      Part{Path: "1", ContentType: "text/plain"},
			mediaType: "text/plain",
		})
	}

	for _, l := range w.leaves {
		out.AllParts = append(out.AllParts, l.part)
		if isAttachmentPart(l) {
			out.Attachments = append(out.Attachments, l.part)
		}
		if out.Calendar == nil && isCalendarPart(l.mediaType) {
			out.Calendar = &CalendarPart{Path: l.part.Path, Method: l.method}
		}
	}
	selectBody(out, w.leaves)
	return out, nil
}

// isCalendarPart reports whether a media type carries iCalendar text.
// text/calendar is what RFC 6047 says an iTIP message travels as;
// application/ics is the copy Google Calendar attaches beside it, and the
// only form some senders use.
func isCalendarPart(mt string) bool {
	return mt == "text/calendar" || mt == "application/ics"
}

// fallbackParse treats raw as a bare text body with no headers.
func fallbackParse(raw []byte) *Parsed {
	body := decodeBytes(raw)
	return &Parsed{
		TextBody: normalizeText(body),
		AllParts: []Part{{Path: "1", ContentType: "text/plain", Size: int64(len(raw))}},
	}
}

// selectBody picks the display body: the first text/plain leaf that is not an
// attachment and has something in it, else the first text/html leaf converted
// to text.
//
// "Has something in it" is the whole of the rule, and it is there because an
// empty text/plain part is common. Apple Mail opens a message with a blank one
// before the first inline image and puts the words in a second part after it;
// Mailspring and several webmail clients send a multipart/alternative whose
// plain half is left empty and write the message only in the HTML. Taking
// either at face value archives the mail with no body at all -- nothing in the
// reader, nothing to quote in a reply, and nothing for search to match.
func selectBody(out *Parsed, leaves []leaf) {
	var plain string
	var html, container *leaf
	for i := range leaves {
		l := &leaves[i]
		if isAttachmentPart(*l) {
			continue
		}
		switch {
		case l.mediaType == "text/plain":
			if plain == "" {
				if s := normalizeText(decodeBytes(l.text)); strings.TrimSpace(s) != "" {
					plain = s
				}
			}
		case l.mediaType == "text/html":
			if html == nil {
				html = l
			}
		case strings.HasPrefix(l.mediaType, "multipart/") && len(l.text) > 0:
			if container == nil {
				container = l
			}
		}
	}
	if html != nil {
		out.HasHTML = true
		out.HTMLPart = html.part.Path
	}
	switch {
	case plain != "":
		out.TextBody = plain
	case html != nil:
		out.TextBody = HTMLToText(decodeBytes(html.text))
	case container != nil:
		// A multipart this parser refused to descend into: past maxDepth, or
		// with a boundary nothing could be read from. Index its bytes rather
		// than archive the message with nothing searchable at all.
		out.TextBody = containerText(decodeBytes(container.text))
	}
}

var (
	reMIMEBoundary = regexp.MustCompile(`^\s*--\S`)
	reMIMEHeader   = regexp.MustCompile(`(?i)^\s*(content-[a-z-]+|mime-version)\s*:`)
)

// containerText salvages readable text from a multipart body that was never
// split into parts, dropping the boundary lines and part headers that would
// otherwise be indexed as if they were prose.
func containerText(s string) string {
	lines := strings.Split(reCRLF.Replace(s), "\n")
	out := lines[:0]
	for _, l := range lines {
		if reMIMEBoundary.MatchString(l) || reMIMEHeader.MatchString(l) {
			continue
		}
		out = append(out, l)
	}
	return normalizeText(strings.Join(out, "\n"))
}

func isAttachmentPart(l leaf) bool {
	switch {
	case l.part.Disposition == "attachment":
		return true
	case l.part.Filename != "" && !strings.HasPrefix(l.mediaType, "text/"):
		return true
	case l.part.Inline && l.part.ContentID != "" && !strings.HasPrefix(l.mediaType, "text/"):
		return true
	case isAttachedMessage(l.mediaType):
		// A forwarded message attached as a bare message/rfc822 part, which is
		// what Apple Mail and several webmail clients send: no disposition and
		// no filename, but an attachment all the same.
		return true
	case isCalendarPart(l.mediaType):
		// The .ics of an invitation. Outlook and Exchange put it in the
		// multipart/alternative next to the text, with no name and no
		// disposition, and it is the whole point of the message: listing it
		// is what lets the index say "this one carries an invite" and lets
		// `mail attachment get` hand the file to another calendar.
		return true
	}
	return false
}

// isAttachedMessage reports whether a leaf of this media type is an attached
// message. The status reports are excluded: they are machine-readable text that
// belongs to the containing report, not a file a user would download.
func isAttachedMessage(mt string) bool {
	if !strings.HasPrefix(mt, "message/") {
		return false
	}
	switch mt {
	case "message/delivery-status", "message/global-delivery-status",
		"message/disposition-notification", "message/global-disposition-notification",
		"message/feedback-report", "message/external-body":
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Tree walking

type walker struct {
	leaves []leaf
	// target, when non-empty, restricts content retention to that part path.
	target string
	found  *leaf
}

func (w *walker) walk(e *message.Entity, path string, depth int) {
	if e == nil || len(w.leaves) >= maxParts {
		return
	}
	mt, params := contentType(&e.Header)

	if strings.HasPrefix(mt, "multipart/") && depth < maxDepth && params["boundary"] != "" {
		if mr := e.MultipartReader(); mr != nil {
			n := 0
			for len(w.leaves) < maxParts {
				child, err := mr.NextPart()
				if child == nil {
					// io.EOF, a malformed boundary, or a truncated body.
					break
				}
				_ = err // unknown charset/encoding: keep going with the part
				n++
				cp := strconv.Itoa(n)
				if path != "" {
					cp = path + "." + cp
				}
				w.walk(child, cp, depth+1)
			}
			if n > 0 {
				return
			}
			// A multipart with no readable parts: fall through and record the
			// raw body as a single leaf so nothing is silently lost.
		}
	}

	if path == "" {
		path = "1"
	}
	w.addLeaf(e, path, mt, params)
}

func (w *walker) addLeaf(e *message.Entity, path, mt string, params map[string]string) {
	disp, cid, inline, filename := dispositionOf(&e.Header, mt)
	if filename == "" && isCalendarPart(mt) {
		// Named the way Google Calendar names the copy it attaches, so the
		// file reads as what it is wherever it ends up.
		filename = "invite.ics"
	}

	l := leaf{
		part: Part{
			Path:        path,
			ContentType: mt,
			Filename:    filename,
			ContentID:   cid,
			Inline:      inline,
			Disposition: disp,
		},
		mediaType: mt,
		method:    strings.ToUpper(strings.TrimSpace(params["method"])),
	}

	// A multipart leaf is a container this walker could not descend into (past
	// maxDepth, or an unreadable boundary); its bytes are the only body left.
	keep := strings.HasPrefix(mt, "text/") || strings.HasPrefix(mt, "multipart/")
	if w.target != "" {
		keep = path == w.target
	}
	// An attached message with no filename gets one from its own Subject, which
	// means reading its header block.
	head := 0
	if !keep && filename == "" && isAttachedMessage(mt) {
		head = maxHeaderPeek
	}

	n, data := drain(e.Body, keep, head)
	l.part.Size = n
	switch {
	case keep:
		l.text = data
	case head > 0:
		l.part.Filename = attachedMessageName(data)
	}
	w.leaves = append(w.leaves, l)
	if w.target != "" && path == w.target {
		cp := l
		w.found = &cp
	}
}

// drain reads a part body and returns its decoded size. It retains the whole
// body (up to maxTextBytes) when keep is set, else the first head bytes when
// head is positive, else nothing.
func drain(r io.Reader, keep bool, head int) (int64, []byte) {
	if r == nil {
		return 0, nil
	}
	lr := io.LimitReader(r, maxPartBytes)
	want := int64(head)
	if keep {
		want = maxTextBytes
	}
	if want <= 0 {
		n, _ := io.Copy(io.Discard, lr)
		return n, nil
	}
	var buf bytes.Buffer
	n, _ := io.Copy(&buf, io.LimitReader(lr, want))
	rest, _ := io.Copy(io.Discard, lr)
	return n + rest, buf.Bytes()
}

// attachedMessageName names an attached message after its own Subject, the way
// a mail client does when it saves one to disk.
func attachedMessageName(head []byte) string {
	subject := ""
	for _, raw := range strings.Split(reCRLF.Replace(decodeBytes(head)), "\n") {
		if strings.TrimSpace(raw) == "" {
			break // end of the attached message's header block
		}
		if subject != "" {
			if raw[0] == ' ' || raw[0] == '\t' {
				subject += " " + strings.TrimSpace(raw) // folded continuation
				continue
			}
			break
		}
		if v, ok := cutHeader(raw, "subject"); ok {
			subject = v
		}
	}
	name := sanitizeFilename(decodeWord(subject))
	if len(name) > maxAttachedNameLen {
		name = strings.TrimSpace(name[:maxAttachedNameLen])
	}
	if name == "" {
		return "forwarded.eml"
	}
	return name + ".eml"
}

// cutHeader returns the value of raw when it is the named header field.
func cutHeader(raw, name string) (string, bool) {
	i := strings.IndexByte(raw, ':')
	if i < 0 || !strings.EqualFold(strings.TrimSpace(raw[:i]), name) {
		return "", false
	}
	return strings.TrimSpace(raw[i+1:]), true
}

var reMediaType = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+/[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+$`)

// contentType returns a lowercase media type and its parameters, defaulting to
// text/plain when the header is missing or unparseable.
func contentType(h *message.Header) (string, map[string]string) {
	mt, params, err := h.ContentType()
	if err != nil {
		// go-message hands back the raw value on error; salvage the type.
		raw := h.Get("Content-Type")
		mt = strings.TrimSpace(strings.SplitN(raw, ";", 2)[0])
		params = map[string]string{}
		if b := boundaryOf(raw); b != "" {
			params["boundary"] = b
		}
	}
	mt = strings.ToLower(strings.TrimSpace(mt))
	if !reMediaType.MatchString(mt) {
		mt = "text/plain"
	}
	if params == nil {
		params = map[string]string{}
	}
	return mt, params
}

var reBoundary = regexp.MustCompile(`(?i)boundary\s*=\s*("([^"]*)"|[^;\s]+)`)

func boundaryOf(raw string) string {
	m := reBoundary.FindStringSubmatch(raw)
	if m == nil {
		return ""
	}
	if m[2] != "" {
		return m[2]
	}
	return strings.Trim(m[1], `"`)
}

var reFilenameParam = regexp.MustCompile(`(?i)\b(?:file)?name\s*=\s*("([^"]*)"|[^;]+)`)

// dispositionOf extracts disposition, Content-ID, inline flag and filename.
func dispositionOf(h *message.Header, mt string) (disp, cid string, inline bool, filename string) {
	d, dparams, derr := h.ContentDisposition()
	if derr != nil {
		d = strings.TrimSpace(strings.SplitN(h.Get("Content-Disposition"), ";", 2)[0])
		dparams = nil
	}
	disp = strings.ToLower(strings.TrimSpace(d))
	if disp != "attachment" && disp != "inline" {
		disp = ""
	}
	inline = disp == "inline"

	if dparams != nil {
		filename = dparams["filename"]
	}
	if filename == "" {
		if _, cparams, err := h.ContentType(); err == nil && cparams != nil {
			filename = cparams["name"]
		}
	}
	if filename == "" {
		// Last resort for headers mime.ParseMediaType refused.
		for _, raw := range []string{h.Get("Content-Disposition"), h.Get("Content-Type")} {
			if m := reFilenameParam.FindStringSubmatch(raw); m != nil {
				filename = strings.Trim(strings.TrimSpace(m[1]), `"`)
				if filename != "" {
					break
				}
			}
		}
	}
	filename = sanitizeFilename(decodeWord(filename))

	cid = strings.TrimSpace(h.Get("Content-Id"))
	cid = strings.TrimSuffix(strings.TrimPrefix(cid, "<"), ">")

	// An image with a Content-ID inside multipart/related is inline even when
	// the disposition header is missing.
	if disp == "" && cid != "" && strings.HasPrefix(mt, "image/") {
		inline = true
	}
	return disp, cid, inline, filename
}

var reBadFilename = regexp.MustCompile(`[\x00-\x1f\x7f]`)

func sanitizeFilename(s string) string {
	s = reBadFilename.ReplaceAllString(s, "")
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSpace(s)
	if s == "." || s == ".." {
		return ""
	}
	if len(s) > 255 {
		s = s[:255]
	}
	return s
}

// ---------------------------------------------------------------------------
// Headers

func readHeaders(h *message.Header, out *Parsed) {
	mh := gomail.Header{Header: *h}

	out.Subject = decodeWord(h.Get("Subject"))

	if id, err := mh.MessageID(); err == nil && id != "" {
		out.MessageID = id
	} else {
		out.MessageID = stripAngles(h.Get("Message-Id"))
	}

	if l, err := mh.MsgIDList("In-Reply-To"); err == nil && len(l) > 0 {
		out.InReplyTo = l[0]
	} else {
		out.InReplyTo = stripAngles(firstField(h.Get("In-Reply-To")))
	}

	if l, err := mh.MsgIDList("References"); err == nil && len(l) > 0 {
		out.References = l
	} else {
		out.References = splitRefs(h.Get("References"))
	}

	out.From = firstAddr(parseAddressList(h.Get("From")))
	out.To = parseAddressList(h.Get("To"))
	out.Cc = parseAddressList(h.Get("Cc"))
	out.Bcc = parseAddressList(h.Get("Bcc"))
	out.ReplyTo = parseAddressList(h.Get("Reply-To"))

	out.Date = parseDate(h.Get("Date"))

	out.ListID = strings.TrimSpace(decodeWord(h.Get("List-Id")))
	out.AutoSubmitted = strings.ToLower(strings.TrimSpace(strings.SplitN(h.Get("Auto-Submitted"), ";", 2)[0]))
	out.Precedence = strings.ToLower(strings.TrimSpace(h.Get("Precedence")))

	out.IsBulk = out.ListID != "" ||
		(out.AutoSubmitted != "" && out.AutoSubmitted != "no") ||
		out.Precedence == "bulk" || out.Precedence == "list" || out.Precedence == "junk"
	if !out.IsBulk && h.Get("List-Unsubscribe") != "" && h.Get("List-Post") != "" {
		out.IsBulk = true
	}
}

func stripAngles(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j > 0 {
			return strings.TrimSpace(s[i+1 : i+j])
		}
	}
	return s
}

func firstField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func splitRefs(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		f = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(f), "<"), ">")
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func firstAddr(l []model.Address) model.Address {
	if len(l) == 0 {
		return model.Address{}
	}
	return l[0]
}

var reEmail = regexp.MustCompile(`[^\s<>,;:"()\[\]]+@[^\s<>,;:"()\[\]]+`)

// parseAddressList parses an address header, falling back to a lenient scan
// for the many real-world headers that net/mail rejects, such as
// `"Name" name@example.com` or a bare display name.
func parseAddressList(raw string) []model.Address {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if l, err := gomail.ParseAddressList(raw); err == nil && len(l) > 0 {
		out := make([]model.Address, 0, len(l))
		for _, a := range l {
			if a == nil {
				continue
			}
			out = append(out, model.Address{Name: unfold(a.Name), Email: strings.TrimSpace(a.Address)})
		}
		if len(out) > 0 {
			return out
		}
	}
	var out []model.Address
	for _, chunk := range splitAddresses(raw) {
		if a, ok := lenientAddress(chunk); ok {
			out = append(out, a)
		}
	}
	return out
}

// splitAddresses splits on commas that are not inside quotes or angle brackets.
func splitAddresses(s string) []string {
	var (
		out    []string
		cur    strings.Builder
		inQ    bool
		inAng  bool
		escape bool
	)
	for _, r := range s {
		switch {
		case escape:
			escape = false
		case r == '\\' && inQ:
			escape = true
		case r == '"':
			inQ = !inQ
		case r == '<' && !inQ:
			inAng = true
		case r == '>' && !inQ:
			inAng = false
		case (r == ',' || r == ';') && !inQ && !inAng:
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}

func lenientAddress(chunk string) (model.Address, bool) {
	chunk = strings.TrimSpace(chunk)
	if chunk == "" {
		return model.Address{}, false
	}
	if a, err := gomail.ParseAddress(chunk); err == nil && a != nil {
		return model.Address{Name: unfold(a.Name), Email: strings.TrimSpace(a.Address)}, true
	}
	// Group syntax: "Undisclosed recipients:;"
	if strings.HasSuffix(chunk, ":;") || strings.HasSuffix(chunk, ": ;") {
		return model.Address{Name: decodeWord(strings.TrimRight(chunk, ":; "))}, true
	}

	var email, rest string
	if i := strings.Index(chunk, "<"); i >= 0 {
		if j := strings.Index(chunk[i:], ">"); j > 0 {
			email = strings.TrimSpace(chunk[i+1 : i+j])
			rest = chunk[:i] + chunk[i+j+1:]
		}
	}
	if email == "" {
		if m := reEmail.FindStringIndex(chunk); m != nil {
			email = chunk[m[0]:m[1]]
			rest = chunk[:m[0]] + chunk[m[1]:]
		}
	}
	email = strings.Trim(strings.TrimSpace(email), "<>\"' ")
	if email == "" {
		// A bare display name with no address at all.
		return model.Address{Name: decodeWord(strings.TrimRight(chunk, ": "))}, true
	}
	name := strings.TrimSpace(rest)
	name = strings.Trim(name, "\"' \t")
	name = decodeWord(name)
	return model.Address{Name: name, Email: email}, true
}

// ---------------------------------------------------------------------------
// Dates

var dateLayouts = []string{
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 -0700",
	"2 Jan 2006 15:04:05 MST",
	"Mon, 2 Jan 2006 15:04 -0700",
	"Mon, 2 Jan 2006 15:04 MST",
	"2 Jan 2006 15:04 -0700",
	"Mon, 2 Jan 06 15:04:05 -0700",
	"Mon, 2 Jan 06 15:04:05 MST",
	"Mon Jan 2 15:04:05 2006",     // ctime
	"Mon Jan 2 15:04:05 MST 2006", // unix date
	"Mon, 2 Jan 2006 15:04:05",    // no zone
	"2 Jan 2006 15:04:05",         // no day, no zone
	"Mon, 2 January 2006 15:04:05 -0700",
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	time.RFC3339,
	// Broken mailers that write a locale date. Slash dates are read as
	// month/day/year, which is the convention of the mailers that emit them.
	"1/2/2006 15:04:05 -0700",
	"1/2/2006 15:04:05",
	"1/2/2006 15:04",
	"1/2/2006",
	"1/2/06 15:04",
}

var reParen = regexp.MustCompile(`\s*\([^()]*\)`)

// parseDate parses a Date header, tolerating the usual real-world damage. It
// returns the zero time when nothing works.
func parseDate(v string) time.Time {
	v = unfold(v)
	if v == "" {
		return time.Time{}
	}
	if t, err := netmail.ParseDate(v); err == nil {
		return t
	}
	// Strip comments such as "(CEST)" and any trailing junk.
	c := strings.TrimSpace(reParen.ReplaceAllString(v, ""))
	c = strings.Join(strings.Fields(c), " ")
	if t, err := netmail.ParseDate(c); err == nil {
		return t
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, c); err == nil {
			return t
		}
	}
	// Some mailers write a numeric zone without a sign, or "UT"/"GMT+0000".
	if fixed := fixZone(c); fixed != c {
		if t, err := netmail.ParseDate(fixed); err == nil {
			return t
		}
		for _, layout := range dateLayouts {
			if t, err := time.Parse(layout, fixed); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

var reNakedZone = regexp.MustCompile(`(\d{2}:\d{2}(:\d{2})?)\s+(\d{4})$`)

func fixZone(s string) string {
	s = strings.TrimSuffix(s, ",")
	s = strings.Replace(s, " UT", " +0000", 1)
	s = reNakedZone.ReplaceAllString(s, "$1 +$3")
	return s
}

// ---------------------------------------------------------------------------
// PartContent

// PartContent re-parses raw and returns the decoded content of the leaf at
// partPath, together with its media type and filename. It returns
// model.ErrNotFound when the path does not name a leaf.
func PartContent(raw []byte, partPath string) (data []byte, contentType, filename string, err error) {
	defer func() {
		if r := recover(); r != nil {
			data, contentType, filename = nil, "", ""
			err = fmt.Errorf("%w: part %q", model.ErrNotFound, partPath)
		}
	}()

	partPath = strings.TrimSpace(partPath)
	if partPath == "" || len(bytes.TrimSpace(raw)) == 0 {
		return nil, "", "", fmt.Errorf("%w: part %q", model.ErrNotFound, partPath)
	}
	ent, _ := message.Read(bytes.NewReader(raw))
	if ent == nil {
		// Parse falls back to treating an unreadable message as one text part;
		// PartContent has to agree with it.
		if partPath == "1" {
			return raw, "text/plain", "", nil
		}
		return nil, "", "", fmt.Errorf("%w: part %q", model.ErrNotFound, partPath)
	}
	w := &walker{target: partPath}
	w.walk(ent, "", 0)
	if w.found == nil {
		if partPath == "1" && len(w.leaves) == 0 {
			return []byte{}, "text/plain", "", nil
		}
		return nil, "", "", fmt.Errorf("%w: part %q", model.ErrNotFound, partPath)
	}
	f := w.found
	out := f.text
	if out == nil {
		out = []byte{}
	}
	return out, f.part.ContentType, f.part.Filename, nil
}
