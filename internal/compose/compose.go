// Package compose turns a message being read into a reply to it: the subject,
// the recipients, the threading headers and the quoted original, plus the
// address parsing and the SMTP envelope every outgoing message needs.
//
// It is a package rather than a few functions in internal/cli because there
// are two surfaces that compose. The CLI owned all of this while `mail reply`
// was the only way to answer a message; the TUI cannot import internal/cli —
// the tui command is registered there, so the dependency runs the other way —
// and a second copy is exactly how the two would come to disagree about what
// a reply looks like.
//
// Nothing here talks to the store, the engine or a provider: it is pure
// transformation, so both surfaces can call it wherever they already hold the
// message.
package compose

import (
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
)

// Reply fills in the header half of a reply to orig: the subject, the
// recipients and the threading headers that put it in the same conversation.
//
// The body is deliberately not touched. The two surfaces assemble it
// differently — `mail reply` appends the quoted original to the text it was
// given on the command line, while the TUI puts the quote in the editor up
// front, so it can be read and cut before it goes — and both build it out of
// [Quote].
//
// Recipients already on the draft win: an address the caller put there is a
// choice, not a default to improve on. all is reply-to-all, which keeps the
// other recipients except the ones in self — the addresses belonging to the
// person replying, who should not be sent a copy of their own reply.
func Reply(draft *mime.Draft, orig *model.Message, all bool, self []string) {
	if draft.Subject == "" {
		draft.Subject = ReplySubject(orig.Subject)
	}

	// Reply-To when the sender asked for it, else the sender.
	if len(draft.To) == 0 {
		switch {
		case len(orig.ReplyTo) > 0:
			draft.To = append(draft.To, orig.ReplyTo...)
		case orig.From.Email != "":
			draft.To = append(draft.To, orig.From)
		}
	}
	if all {
		mine := map[string]bool{}
		for _, e := range self {
			if e = strings.TrimSpace(e); e != "" {
				mine[strings.ToLower(e)] = true
			}
		}
		draft.To = MergeAddresses(draft.To, orig.To, mine)
		draft.Cc = MergeAddresses(draft.Cc, orig.Cc, mine)
	}

	draft.InReplyTo = orig.MessageIDHeader
	draft.References = append(append([]string{}, orig.References...), orig.MessageIDHeader)
}

// ReplySubject prefixes "Re: ", and does not do so twice.
func ReplySubject(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(s), "re:") {
		return s
	}
	return "Re: " + s
}

// Quote renders the attribution line and the quoted original body. What is
// quoted is what the sender actually typed: the rounds before theirs are
// stripped out, or every reply would carry the whole thread twice over.
func Quote(orig *model.Message, loc *time.Location) string {
	when := orig.Date
	if when.IsZero() {
		when = orig.Received
	}
	who := orig.From.Name
	if who == "" {
		who = orig.From.Email
	}
	var b strings.Builder
	fmt.Fprintf(&b, "On %s, %s wrote:\n", when.In(loc).Format("Mon, 02 Jan 2006 at 15:04"), who)
	for _, line := range strings.Split(strings.TrimRight(mime.StripQuotes(orig.TextBody), "\n"), "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// ForwardSubject prefixes "Fwd: ", and does not do so twice. Someone else's
// "Fw:" counts as already done: re-prefixing it would say the message had been
// forwarded twice when it has been forwarded once.
func ForwardSubject(s string) string {
	s = strings.TrimSpace(s)
	l := strings.ToLower(s)
	if strings.HasPrefix(l, "fwd:") || strings.HasPrefix(l, "fw:") {
		return s
	}
	return "Fwd: " + s
}

// Forwarded renders the message being passed on: the header block every mail
// client writes above a forward, then the original body.
//
// Verbatim is the whole difference from [Quote]. A reply quotes what the
// sender typed and strips the rounds before it, because the person being
// answered wrote those and has them already; a forward hands the conversation
// to somebody who has seen none of it, so nothing is stripped and no line is
// marked "> " -- the header block is what says where the text came from, and
// the quoting inside it is the original's own.
//
// Attachments are not here. They are not in the text body, and the archive
// holds a reference rather than the bytes for most of them, so a forward built
// from the index alone carries the words and not the files. Saying so is the
// composer's job: this is a pure transformation like everything else in the
// package, and it can only pass on what it was given.
func Forwarded(orig *model.Message, loc *time.Location) string {
	when := orig.Date
	if when.IsZero() {
		when = orig.Received
	}
	var b strings.Builder
	b.WriteString("---------- Forwarded message ----------\n")
	fmt.Fprintf(&b, "From: %s\n", JoinAddresses([]model.Address{orig.From}))
	fmt.Fprintf(&b, "Date: %s\n", when.In(loc).Format("Mon, 02 Jan 2006 at 15:04"))
	fmt.Fprintf(&b, "Subject: %s\n", strings.TrimSpace(orig.Subject))
	if to := JoinAddresses(orig.To); to != "" {
		fmt.Fprintf(&b, "To: %s\n", to)
	}
	if cc := JoinAddresses(orig.Cc); cc != "" {
		fmt.Fprintf(&b, "Cc: %s\n", cc)
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimRight(orig.TextBody, "\n"))
	b.WriteString("\n")
	return b.String()
}

// ForwardAttachments is the files a forward carries: everything on the
// original except the inline parts its body refers to by Content-ID.
//
// Those are the signature logo and the images laid into the HTML -- part of
// how the message looked, not things anybody meant to pass on. A forward is
// built out of the text body, which no longer refers to them at all, so
// carrying them would turn somebody's letterhead into two files the recipient
// has to wonder about. An attachment marked inline with nothing referring to
// it is an ordinary attachment: plenty of clients send a PDF that way.
func ForwardAttachments(atts []model.Attachment) []model.Attachment {
	out := make([]model.Attachment, 0, len(atts))
	for _, a := range atts {
		if a.Inline && a.ContentID != "" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// MergeAddresses appends add to base, dropping the addresses in skip and any
// duplicates (case-insensitive on the address).
func MergeAddresses(base, add []model.Address, skip map[string]bool) []model.Address {
	seen := map[string]bool{}
	for _, a := range base {
		seen[strings.ToLower(a.Email)] = true
	}
	for _, a := range add {
		k := strings.ToLower(a.Email)
		if k == "" || seen[k] || skip[k] {
			continue
		}
		seen[k] = true
		base = append(base, a)
	}
	return base
}

// Envelope is every address the message must actually be delivered to: To, Cc
// and Bcc together, deduplicated. Bcc is included here precisely because it is
// not in the message bytes — the header must not reach the recipients — so a
// submission over SMTP would otherwise have nothing to put in RCPT TO.
func Envelope(d *mime.Draft) []string {
	var out []string
	seen := map[string]bool{}
	for _, group := range [][]model.Address{d.To, d.Cc, d.Bcc} {
		for _, a := range group {
			e := strings.TrimSpace(a.Email)
			if e == "" || seen[strings.ToLower(e)] {
				continue
			}
			seen[strings.ToLower(e)] = true
			out = append(out, e)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Addresses

// ParseAddress accepts "Name <a@b>" and a bare "a@b".
func ParseAddress(s string) (model.Address, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return model.Address{}, fmt.Errorf("empty address")
	}
	a, err := mail.ParseAddress(s)
	if err != nil {
		// A bare address without angle brackets that ParseAddress rejects
		// (e.g. missing a domain) is a usage error, not a crash.
		return model.Address{}, fmt.Errorf("invalid address %q", s)
	}
	return model.Address{Name: a.Name, Email: a.Address}, nil
}

// ParseAddressList splits repeated and comma-separated values into addresses.
func ParseAddressList(values []string) ([]model.Address, error) {
	var out []model.Address
	for _, v := range values {
		for _, part := range SplitAddresses(v) {
			a, err := ParseAddress(part)
			if err != nil {
				return nil, err
			}
			out = append(out, a)
		}
	}
	return out, nil
}

// SplitAddresses splits on commas that are not inside a quoted display name,
// so `Doe, Jane <j@x>,b@y` does the obvious thing.
func SplitAddresses(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			if t := strings.TrimSpace(cur.String()); t != "" {
				parts = append(parts, t)
			}
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if t := strings.TrimSpace(cur.String()); t != "" {
		parts = append(parts, t)
	}
	return parts
}

// JoinAddresses renders addresses back into one header-style line, which is
// what an editable To/Cc field holds.
func JoinAddresses(addrs []model.Address) string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if s := FormatAddress(a); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}

// FormatAddress writes one address for a field a person edits and the parser
// reads back.
//
// model.Address.String() is not it: that is a display formatter, and it leaves
// a display name containing a comma unquoted -- exactly the line
// [ParseAddressList] would then read as two addresses, one of them rubbish.
// net/mail alone is not it either: it quotes any name with a space in it, so
// every ordinary "Anna de Vries" arrives in the To field wearing quotes.
//
// So the plain form is written and then checked, and only a name that does not
// survive being parsed back -- one carrying a comma, angle brackets, a quote --
// falls back to net/mail's escaping.
func FormatAddress(a model.Address) string {
	email := strings.TrimSpace(a.Email)
	name := strings.TrimSpace(a.Name)
	switch {
	case email == "":
		return ""
	case name == "":
		// net/mail renders a nameless address as "<a@b>"; bare is what a
		// person expects to see in a To field.
		return email
	}
	plain := name + " <" + email + ">"
	if back, err := ParseAddress(plain); err == nil && back.Name == name && back.Email == email {
		return plain
	}
	return (&mail.Address{Name: name, Address: email}).String()
}
