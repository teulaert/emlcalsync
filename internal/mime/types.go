// Package mime parses raw RFC 822 messages into the indexed representation,
// extracts readable text, strips quoted replies/signatures for display, and
// builds RFC 822 messages for sending.
//
// Required exported API (implemented in this package):
//
//	func Parse(raw []byte) (*Parsed, error)
//	    Never fails on malformed input if a best-effort result is possible;
//	    returns an error only when raw is not a message at all.
//
//	func StripQuotes(text string) string
//	    Removes quoted replies and signatures for display. Pure function.
//
//	func Snippet(text string, n int) string
//	    First n runes of text with whitespace collapsed, for list output.
//
//	func PartContent(raw []byte, partPath string) (data []byte, contentType, filename string, err error)
//	    Decodes one MIME leaf (transfer-encoding removed) by its part path.
//
//	type Draft struct { From model.Address; To, Cc, Bcc []model.Address; Subject string;
//	                    TextBody string; InReplyTo string; References []string;
//	                    Attachments []DraftAttachment; Date time.Time; MessageID string }
//	type DraftAttachment struct { Filename, ContentType string; Data []byte }
//	func Build(d *Draft) ([]byte, error)
//	    Produces a well-formed multipart RFC 822 message (7bit-safe, CRLF).
package mime

import (
	"time"

	"github.com/lennert/emlcal/internal/model"
)

// Parsed is the result of Parse.
type Parsed struct {
	MessageID     string // without <>
	InReplyTo     string
	References    []string
	Subject       string
	From          model.Address
	To            []model.Address
	Cc            []model.Address
	Bcc           []model.Address
	ReplyTo       []model.Address
	Date          time.Time // zero if missing/unparseable
	ListID        string
	AutoSubmitted string
	Precedence    string
	// IsBulk is true for list mail / auto-submitted / bulk precedence.
	IsBulk bool

	// TextBody is the extracted plain text (from text/plain, else html→text).
	TextBody string
	// HasHTML reports whether an HTML alternative exists (see HTMLPart).
	HasHTML  bool
	HTMLPart string // part path of the html body, "" if none

	Attachments []Part
	// AllParts lists every leaf part in tree order (for debugging / --headers).
	AllParts []Part
}

// Part describes a MIME leaf.
type Part struct {
	Path        string // "1", "1.2", ...
	ContentType string
	Filename    string
	Size        int64 // decoded size if known, else encoded size
	ContentID   string
	Inline      bool
	Disposition string
}
