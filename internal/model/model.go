// Package model holds the provider-neutral domain types shared by every other
// package. Nothing in here knows about SQLite, Gmail, or JMAP.
//
// The mail model is the JMAP model: a message belongs to many mailboxes and
// carries a small set of flags. Gmail labels are mapped onto mailboxes and
// flags at index time.
package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Provider identifies an account backend.
type Provider string

const (
	ProviderGmail    Provider = "gmail"    // Gmail API + Google Calendar API
	ProviderFastmail Provider = "fastmail" // JMAP mail + JMAP calendars
)

// Account is a configured account. ID is the short name from config.toml
// ("work", "personal") and is the prefix of every public ID.
type Account struct {
	ID        string
	Provider  Provider
	Email     string
	CreatedAt time.Time
}

// MailboxRole is the normalised role of a mailbox/label. Empty means a
// user-created label/folder.
type MailboxRole string

const (
	RoleInbox     MailboxRole = "inbox"
	RoleArchive   MailboxRole = "archive"
	RoleSent      MailboxRole = "sent"
	RoleDrafts    MailboxRole = "drafts"
	RoleTrash     MailboxRole = "trash"
	RoleJunk      MailboxRole = "junk"
	RoleImportant MailboxRole = "important"
	RoleAll       MailboxRole = "all" // "All Mail" style virtual mailbox
	// Gmail categories are stored as "category:<name>" (e.g. category:promotions).
)

// Mailbox is a folder (JMAP) or label (Gmail).
type Mailbox struct {
	ID          int64  // local row id, 0 when not yet stored
	AccountID   string
	RemoteID    string
	Name        string
	Role        MailboxRole
	ParentRemote string // remote id of parent, "" for top-level
	SortOrder   int
	TotalCount  int
	UnreadCount int
}

// Address is a parsed RFC 5322 mailbox.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

func (a Address) String() string {
	if a.Name == "" {
		return a.Email
	}
	return fmt.Sprintf("%s <%s>", a.Name, a.Email)
}

// Flags are the normalised per-message flags.
// Gmail: UNREAD/STARRED/DRAFT labels. JMAP: $seen/$flagged/$draft/$answered.
type Flags struct {
	Unread   bool `json:"unread"`
	Flagged  bool `json:"flagged"`
	Draft    bool `json:"draft"`
	Answered bool `json:"answered"`
}

// Message is the indexed form of a mail message.
type Message struct {
	ID          int64  // local row id
	AccountID   string
	RemoteID    string // Gmail message id / JMAP Email id
	ThreadID    string // provider thread id
	BlobSHA256  string // "" when RawComplete is false
	RawComplete bool

	MessageIDHeader string   // Message-ID header value, without <>
	InReplyTo       string
	References      []string

	Subject string
	From    Address
	To      []Address
	Cc      []Address
	Bcc     []Address
	ReplyTo []Address

	Date     time.Time // Date header
	Received time.Time // provider internalDate / receivedAt
	Size     int64
	Snippet  string
	TextBody string // full extracted text incl. quotes

	HasAttachments bool
	Flags          Flags
	MailboxRemotes []string // remote ids of mailboxes this message is in

	DeletedAt *time.Time
	IndexedAt time.Time
}

// PublicID returns the opaque id shown to users/agents: "<account>:<remote>".
func (m *Message) PublicID() string { return MessagePublicID(m.AccountID, m.RemoteID) }

// Attachment is a MIME part recorded at index time. Content is read from the
// blob on demand (PartPath) or fetched remotely (RemoteRef) when the raw
// message was not stored in full.
type Attachment struct {
	ID          int64
	MessageID   int64
	PartPath    string // e.g. "1.2"
	Filename    string
	ContentType string
	Size        int64
	ContentID   string
	Inline      bool
	RemoteRef   string // Gmail attachmentId / JMAP blobId
}

// Thread is a denormalised summary maintained at index time.
type Thread struct {
	AccountID    string
	ThreadID     string
	Subject      string
	First        time.Time
	Last         time.Time
	MessageCount int
	UnreadCount  int
	Participants []Address
}

func (t *Thread) PublicID() string { return ThreadPublicID(t.AccountID, t.ThreadID) }

// ---------------------------------------------------------------------------
// Calendar

type Calendar struct {
	ID         int64
	AccountID  string
	RemoteID   string
	Name       string
	Color      string
	Timezone   string
	Primary    bool
	AccessRole string // owner|writer|reader
}

type EventStatus string

const (
	StatusConfirmed EventStatus = "confirmed"
	StatusTentative EventStatus = "tentative"
	StatusCancelled EventStatus = "cancelled"
)

type Participation string

const (
	PartAccepted    Participation = "accepted"
	PartDeclined    Participation = "declined"
	PartTentative   Participation = "tentative"
	PartNeedsAction Participation = "needs-action"
)

type Attendee struct {
	Name     string        `json:"name,omitempty"`
	Email    string        `json:"email"`
	Response Participation `json:"response,omitempty"`
	Optional bool          `json:"optional,omitempty"`
	Self     bool          `json:"self,omitempty"`
}

// Event is a calendar event master or exception instance.
type Event struct {
	ID           int64
	CalendarID   int64
	AccountID    string // denormalised for PublicID
	CalendarRemote string
	RemoteID     string
	UID          string
	Title        string
	Description  string
	Location     string
	Start        time.Time
	End          time.Time
	AllDay       bool
	Timezone     string
	RRule        string // RFC 5545 RRULE without the "RRULE:" prefix; "" if single
	RecurrenceID string // set on exception instances
	Status       EventStatus
	Organizer    Address
	Attendees    []Attendee
	MyResponse   Participation
	RawJSON      []byte // provider object for fidelity / minimal patches
	Updated      time.Time
	DeletedAt    *time.Time
}

func (e *Event) PublicID() string {
	return EventPublicID(e.AccountID, e.CalendarRemote, e.RemoteID)
}

// Occurrence is one expanded instance of an event.
type Occurrence struct {
	EventID int64
	Start   time.Time
	End     time.Time
}

// ---------------------------------------------------------------------------
// Public IDs
//
//	message: <account>:<remote_id>
//	thread:  <account>:t:<thread_id>
//	event:   <account>:c:<calendar_remote_id>:<event_remote_id>
//
// Remote ids never contain ':' for either provider; account ids are validated
// to [a-z0-9-]. Calendar remote ids are the only component allowed to
// contain ':'-free but otherwise arbitrary text, so events split from the right.

func MessagePublicID(account, remote string) string { return account + ":" + remote }
func ThreadPublicID(account, thread string) string  { return account + ":t:" + thread }
func EventPublicID(account, calendar, event string) string {
	return account + ":c:" + calendar + ":" + event
}

var ErrBadID = errors.New("malformed id")

type IDKind int

const (
	KindMessage IDKind = iota
	KindThread
	KindEvent
)

// ParsedID is the result of ParseID.
type ParsedID struct {
	Kind     IDKind
	Account  string
	Remote   string // message remote id, thread id, or event remote id
	Calendar string // only for KindEvent
}

// ParseID parses any public id.
func ParseID(s string) (ParsedID, error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ParsedID{}, fmt.Errorf("%w: %q", ErrBadID, s)
	}
	switch {
	case parts[1] == "t" && len(parts) == 3 && parts[2] != "":
		return ParsedID{Kind: KindThread, Account: parts[0], Remote: parts[2]}, nil
	case parts[1] == "c" && len(parts) == 3:
		i := strings.LastIndex(parts[2], ":")
		if i <= 0 || i == len(parts[2])-1 {
			return ParsedID{}, fmt.Errorf("%w: %q", ErrBadID, s)
		}
		return ParsedID{Kind: KindEvent, Account: parts[0], Calendar: parts[2][:i], Remote: parts[2][i+1:]}, nil
	case len(parts) == 2:
		return ParsedID{Kind: KindMessage, Account: parts[0], Remote: parts[1]}, nil
	}
	return ParsedID{}, fmt.Errorf("%w: %q", ErrBadID, s)
}

// ValidAccountID reports whether s is usable as an account id.
func ValidAccountID(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Errors shared across packages.

var (
	ErrNotFound = errors.New("not found")
	// ErrOffline is returned/wrapped when a network operation could not reach
	// the provider at all (DNS, connect, timeout) as opposed to a provider
	// rejecting the request.
	ErrOffline = errors.New("offline")
)
