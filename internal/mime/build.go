package mime

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	stdmime "mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"net/textproto"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/teulaert/emlcalsync/internal/model"
)

// Draft is an outgoing message.
type Draft struct {
	From        model.Address
	To          []model.Address
	Cc          []model.Address
	Bcc         []model.Address
	Subject     string
	TextBody    string
	InReplyTo   string   // Message-ID of the message replied to, with or without <>
	References  []string // Message-IDs, with or without <>
	Attachments []DraftAttachment
	Date        time.Time // zero means now
	MessageID   string    // generated when empty

	// IncludeBcc writes the Bcc header into the built message. It is off by
	// default: Bcc recipients belong in the envelope, not in the message every
	// recipient can read. Set it only when the transport needs the header
	// (some JMAP/Gmail submission paths) or for a copy filed in Sent.
	IncludeBcc bool
}

// DraftAttachment is a file to attach.
type DraftAttachment struct {
	Filename    string
	ContentType string // defaults to application/octet-stream
	Data        []byte
}

const (
	// maxHeaderLine is the fold target: writeHeader breaks between tokens once a
	// line would grow past it.
	maxHeaderLine = 76
	// hardFoldLimit is where a single token that cannot be folded at whitespace
	// is split anyway. RFC 5322 caps a line at 998 bytes; staying well under it
	// leaves room for the field name and for a transport that adds a prefix.
	hardFoldLimit = 900
	// maxEncodedWord is the RFC 2047 limit on one encoded word, including the
	// charset, the encoding and the delimiters.
	maxEncodedWord = 75
)

// Build renders d as an RFC 822 message with CRLF line endings and no line
// longer than 998 bytes. The body is text/plain; charset=utf-8 encoded
// quoted-printable, wrapped in multipart/mixed when there are attachments.
func Build(d *Draft) ([]byte, error) {
	if d == nil {
		return nil, errors.New("mime: nil draft")
	}

	date := d.Date
	if date.IsZero() {
		date = time.Now()
	}
	msgID := strings.TrimSpace(stripAngles(d.MessageID))
	if msgID == "" {
		var err error
		if msgID, err = generateMessageID(d.From.Email); err != nil {
			return nil, err
		}
	}

	var h bytes.Buffer
	writeHeader(&h, "Date", date.Format("Mon, 02 Jan 2006 15:04:05 -0700"))
	if d.From.Email != "" || d.From.Name != "" {
		writeHeader(&h, "From", formatAddresses([]model.Address{d.From}))
	}
	if v := formatAddresses(d.To); v != "" {
		writeHeader(&h, "To", v)
	}
	if v := formatAddresses(d.Cc); v != "" {
		writeHeader(&h, "Cc", v)
	}
	if d.IncludeBcc {
		if v := formatAddresses(d.Bcc); v != "" {
			writeHeader(&h, "Bcc", v)
		}
	}
	if s := unfold(d.Subject); s != "" {
		writeHeader(&h, "Subject", encodeHeaderText(s))
	}
	writeHeader(&h, "Message-ID", "<"+msgID+">")
	if v := strings.TrimSpace(stripAngles(d.InReplyTo)); v != "" {
		writeHeader(&h, "In-Reply-To", "<"+v+">")
	}
	if refs := formatRefs(d.References); refs != "" {
		writeHeader(&h, "References", refs)
	}
	writeHeader(&h, "MIME-Version", "1.0")

	body := normalizeText(d.TextBody)
	if body != "" {
		body += "\n"
	}

	var out bytes.Buffer
	if len(d.Attachments) == 0 {
		writeHeader(&h, "Content-Type", "text/plain; charset=utf-8")
		writeHeader(&h, "Content-Transfer-Encoding", "quoted-printable")
		out.Write(h.Bytes())
		out.WriteString("\r\n")
		if err := writeQP(&out, body); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	}

	var parts bytes.Buffer
	mw := multipart.NewWriter(&parts)
	if err := mw.SetBoundary(randomBoundary()); err != nil {
		return nil, err
	}

	tp, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return nil, err
	}
	if err := writeQP(tp, body); err != nil {
		return nil, err
	}

	for _, a := range d.Attachments {
		ct := strings.TrimSpace(a.ContentType)
		if ct == "" {
			ct = "application/octet-stream"
		}
		name := sanitizeFilename(a.Filename)
		hdr := textproto.MIMEHeader{
			"Content-Transfer-Encoding": {"base64"},
		}
		if name != "" {
			hdr.Set("Content-Type", stdmime.FormatMediaType(ct, map[string]string{"name": name}))
			hdr.Set("Content-Disposition", stdmime.FormatMediaType("attachment", map[string]string{"filename": name}))
		} else {
			hdr.Set("Content-Type", ct)
			hdr.Set("Content-Disposition", "attachment")
		}
		pw, err := mw.CreatePart(hdr)
		if err != nil {
			return nil, err
		}
		if err := writeBase64(pw, a.Data); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	writeHeader(&h, "Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	out.Write(h.Bytes())
	out.WriteString("\r\n")
	out.Write(parts.Bytes())
	return out.Bytes(), nil
}

func writeQP(w io.Writer, body string) error {
	qw := quotedprintable.NewWriter(w)
	if _, err := qw.Write([]byte(body)); err != nil {
		return err
	}
	return qw.Close()
}

func writeBase64(w io.Writer, data []byte) error {
	const lineLen = 76
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > lineLen {
		if _, err := w.Write([]byte(enc[:lineLen] + "\r\n")); err != nil {
			return err
		}
		enc = enc[lineLen:]
	}
	if enc != "" {
		if _, err := w.Write([]byte(enc + "\r\n")); err != nil {
			return err
		}
	}
	return nil
}

// writeHeader writes one header field, folded so no line exceeds the RFC 5322
// limit. Folding happens at whitespace, which is safe for encoded words (they
// never contain spaces) and for comma-separated address lists. A token that is
// on its own longer than hardFoldLimit is split anyway: an over-long line is
// rejected outright by strict MTAs, an over-long unstructured value is not.
func writeHeader(buf *bytes.Buffer, key, value string) {
	value = strings.TrimSpace(unfold(value))
	line := key + ":"
	// hardFold splits a token no whitespace could break. The continuation is a
	// fold, so the receiver reassembles it with one space where the break was.
	hardFold := func() {
		for len(line) > hardFoldLimit {
			cut := hardFoldLimit
			for cut > 1 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			buf.WriteString(line[:cut])
			buf.WriteString("\r\n")
			line = " " + line[cut:]
		}
	}
	for _, tok := range strings.Split(value, " ") {
		if tok == "" {
			continue
		}
		if len(line)+1+len(tok) > maxHeaderLine && line != key+":" {
			buf.WriteString(line)
			buf.WriteString("\r\n")
			line = " "
		} else {
			line += " "
		}
		line += tok
		hardFold()
	}
	buf.WriteString(line)
	buf.WriteString("\r\n")
}

// encodeHeaderText prepares an unstructured header value (Subject) for the
// wire. A value that would leave writeHeader with a token too long to fold is
// forced into RFC 2047 encoded words, which are short and separated by spaces,
// so folding works again and no line runs over the limit.
func encodeHeaderText(s string) string {
	if longestToken(s) > maxEncodedWord {
		return encodeWordsForced(s)
	}
	if isPrintableASCII(s) {
		return s
	}
	enc := stdmime.QEncoding.Encode("utf-8", s)
	if longestToken(enc) > maxEncodedWord {
		// A single word that Go declined to split (it never splits an already
		// short input, but a pathological charset could still get here).
		return encodeWordsForced(s)
	}
	return enc
}

// longestToken returns the length in bytes of the longest whitespace-free run
// in s, which is the shortest line writeHeader can fold it onto.
func longestToken(s string) int {
	longest := 0
	for _, tok := range strings.Fields(s) {
		if len(tok) > longest {
			longest = len(tok)
		}
	}
	return longest
}

const encodedWordPrefix = "=?utf-8?q?"

// encodeWordsForced Q-encodes s into RFC 2047 encoded words of at most
// maxEncodedWord bytes each, whether or not s strictly needs encoding.
// mime.WordEncoder returns a value it considers plain ASCII unchanged, which is
// exactly the case that produces an unfoldable header line.
func encodeWordsForced(s string) string {
	budget := maxEncodedWord - len(encodedWordPrefix) - len("?=")
	var (
		words []string
		cur   strings.Builder
	)
	for _, r := range s {
		enc := qEncodeRune(r)
		if cur.Len() > 0 && cur.Len()+len(enc) > budget {
			words = append(words, encodedWordPrefix+cur.String()+"?=")
			cur.Reset()
		}
		cur.WriteString(enc)
	}
	if cur.Len() > 0 {
		words = append(words, encodedWordPrefix+cur.String()+"?=")
	}
	return strings.Join(words, " ")
}

// qEncodeRune renders one rune as RFC 2047 "Q" text. Everything outside a
// conservative safe set is escaped, so the result is legal in both a phrase and
// an unstructured field.
func qEncodeRune(r rune) string {
	if r == ' ' {
		return "_"
	}
	if r < utf8.RuneSelf && isQSafe(byte(r)) {
		return string(r)
	}
	var sb strings.Builder
	var buf [4]byte
	n := utf8.EncodeRune(buf[:], r)
	for _, b := range buf[:n] {
		sb.WriteString(fmt.Sprintf("=%02X", b))
	}
	return sb.String()
}

func isQSafe(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	}
	return strings.IndexByte("!*+-/", b) >= 0
}

func isPrintableASCII(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

func formatAddresses(l []model.Address) string {
	var out []string
	for _, a := range l {
		email := strings.TrimSpace(a.Email)
		if email == "" {
			continue
		}
		na := netmail.Address{Name: unfold(a.Name), Address: email}
		out = append(out, na.String())
	}
	return strings.Join(out, ", ")
}

func formatRefs(refs []string) string {
	var out []string
	for _, r := range refs {
		r = strings.TrimSpace(stripAngles(r))
		if r != "" {
			out = append(out, "<"+r+">")
		}
	}
	return strings.Join(out, " ")
}

func generateMessageID(from string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mime: %w", err)
	}
	host := ""
	if i := strings.LastIndex(from, "@"); i >= 0 {
		host = strings.TrimSpace(from[i+1:])
	}
	if host == "" {
		host, _ = os.Hostname()
	}
	if host == "" || strings.ContainsAny(host, " <>@") {
		host = "localhost"
	}
	return hex.EncodeToString(b[:]) + "@" + host, nil
}

func randomBoundary() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "emlcal-boundary-fallback-0000000000"
	}
	return "emlcal-" + hex.EncodeToString(b[:])
}
