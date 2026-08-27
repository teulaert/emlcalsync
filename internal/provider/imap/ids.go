// Package imap implements provider.MailProvider over IMAP, with SMTP for the
// half IMAP cannot do (DESIGN.md §6.5).
//
// The client is vendor-neutral, the way the CalDAV one is: everything a
// particular server needs that the protocol does not say lives in a Preset
// (presets.go), and a server with no preset is configured by explicit host.
//
// Two things about IMAP shape this package.
//
// A message is (folder, uidvalidity, uid), not an object with an id — so a
// copy or a move mints a new uid and the old one stops naming anything. Remote
// ids are therefore per-copy, and writes report what they moved so the engine
// can rename the row instead of re-fetching it (see provider.Remapper).
//
// And there is no server-side change log. What we last saw is remembered in the
// sync state instead, UID set and all (state.go), which is what lets a
// UIDVALIDITY reset or a deleted folder be reported as exact changes rather
// than forcing a reconcile.
package imap

import (
	"encoding/base32"
	"fmt"
	"strconv"
	"strings"

	imapv2 "github.com/emersion/go-imap/v2"
)

// A remote id is "<folder>.<uidvalidity>.<uid>", where the folder is base32'd.
//
// The encoding is not decoration. model.ParseID splits a public id on ":" and
// requires exactly two parts, so a remote id containing a colon breaks every id
// the CLI prints — and folder names are user-chosen, so they contain anything.
// base32 without padding is [A-Z2-7], which leaves "." unambiguous as the
// separator and keeps the id shell-safe. Lowercased for looks; decoding
// uppercases again.
var mailboxEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// ref locates one copy of a message: the folder it is in, the folder's
// UIDVALIDITY at the time, and the uid within it.
type ref struct {
	Mailbox     string
	UIDValidity uint32
	UID         imapv2.UID
}

// encodeMailbox renders a folder name for use inside a remote id.
func encodeMailbox(name string) string {
	return strings.ToLower(mailboxEnc.EncodeToString([]byte(name)))
}

// decodeMailbox reverses encodeMailbox.
func decodeMailbox(s string) (string, error) {
	b, err := mailboxEnc.DecodeString(strings.ToUpper(s))
	if err != nil {
		return "", fmt.Errorf("imap: bad mailbox in id: %w", err)
	}
	return string(b), nil
}

// String renders the ref as a remote id.
func (r ref) String() string {
	return encodeMailbox(r.Mailbox) + "." +
		strconv.FormatUint(uint64(r.UIDValidity), 10) + "." +
		strconv.FormatUint(uint64(r.UID), 10)
}

// parseRef reads a remote id this package produced.
func parseRef(id string) (ref, error) {
	parts := strings.Split(id, ".")
	if len(parts) != 3 {
		return ref{}, fmt.Errorf("imap: bad message id %q", id)
	}
	mbox, err := decodeMailbox(parts[0])
	if err != nil {
		return ref{}, fmt.Errorf("imap: bad message id %q: %w", id, err)
	}
	validity, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return ref{}, fmt.Errorf("imap: bad uidvalidity in %q: %w", id, err)
	}
	uid, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return ref{}, fmt.Errorf("imap: bad uid in %q: %w", id, err)
	}
	if uid == 0 {
		return ref{}, fmt.Errorf("imap: uid 0 in %q", id)
	}
	return ref{Mailbox: mbox, UIDValidity: uint32(validity), UID: imapv2.UID(uid)}, nil
}

// groupRefs sorts ids by the folder they live in, so a caller can select each
// folder once and act on every id in it. Ids that do not parse are returned
// separately rather than failing the batch: one unreadable id, probably written
// by an older build, must not stop the rest of a write.
func groupRefs(ids []string) (byMailbox map[string][]ref, bad []string) {
	byMailbox = make(map[string][]ref)
	for _, id := range ids {
		r, err := parseRef(id)
		if err != nil {
			bad = append(bad, id)
			continue
		}
		byMailbox[r.Mailbox] = append(byMailbox[r.Mailbox], r)
	}
	return byMailbox, bad
}

// uidSet builds the UID set for a batch of refs in one folder.
func uidSet(refs []ref) imapv2.UIDSet {
	var s imapv2.UIDSet
	for _, r := range refs {
		s.AddNum(r.UID)
	}
	return s
}
