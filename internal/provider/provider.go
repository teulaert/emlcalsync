// Package provider defines the interfaces every backend implements. The sync
// engine talks only to these; the CLI never imports a concrete provider.
//
// Concrete implementations live in subpackages:
//
//	provider/jmap   Fastmail mail + calendars (JMAP, hand-rolled client)
//	provider/gmail  Gmail API
//	provider/gcal   Google Calendar API
//	provider/oauth  Google OAuth loopback flow + token persistence
package provider

import (
	"context"
	"errors"
	"net"
	"syscall"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

// ErrStateExpired is returned by Changes when the server can no longer
// compute a delta from the given state (Gmail history 404, JMAP
// cannotCalculateChanges, Google Calendar 410). The caller must Reconcile.
var ErrStateExpired = errors.New("sync state expired")

// ErrNotSupported is returned when the account or its credentials cannot
// serve a resource at all (e.g. a Fastmail API token without the calendars
// scope). The sync engine skips that resource instead of failing the account.
var ErrNotSupported = errors.New("not supported by this account or token")

// Envelope is the minimal per-message information returned during
// enumeration: enough to decide whether we need to fetch the raw message and
// to apply flags/mailboxes without a fetch.
type Envelope struct {
	RemoteID  string
	ThreadID  string
	Received  time.Time
	Size      int64
	Flags     model.Flags
	Mailboxes []string // remote mailbox/label ids
}

// RawMessage is a full message as fetched from the provider.
type RawMessage struct {
	Envelope
	Raw []byte // complete RFC 822 bytes, exactly as the provider returned them
}

// Rename records a remote id that changed while the message itself stayed the
// same: an IMAP COPY/MOVE hands the copy a new UID, and a folder rename does it
// to every message at once. The engine rewrites the index row in place instead
// of deleting it and fetching the same bytes again under a new id.
//
// Backends whose ids never move (Gmail, JMAP) never produce one.
type Rename struct{ Old, New string }

// Changes is the result of a delta call.
type Changes struct {
	// Renamed are ids that moved. The engine applies these BEFORE it interprets
	// Added/Updated/Removed, so the rest of the delta may refer to the new ids.
	// A folder rename must report its messages here and set MailboxesChanged:
	// applying the renames first is what keeps ReplaceMailboxes from cascading
	// the old mailbox's memberships away.
	Renamed []Rename
	// Added are messages that are new since the state. The sync engine will
	// FetchRaw them. Only RemoteID is required; other fields are optional hints.
	Added []Envelope
	// Updated are messages whose flags/mailboxes changed. Flags and Mailboxes
	// MUST be populated (current server values).
	Updated []Envelope
	// Removed are remote ids that no longer exist.
	Removed []string
	// MailboxesChanged is a hint that the mailbox list should be re-synced.
	MailboxesChanged bool
	// NewState is the state to persist once everything above is applied.
	NewState string
}

// MailProvider is implemented by every mail backend.
type MailProvider interface {
	// Mailboxes returns the full current mailbox/label list.
	Mailboxes(ctx context.Context) ([]model.Mailbox, error)

	// State returns the provider's current delta state token. Called before a
	// backfill starts so changes made during the backfill can be replayed.
	State(ctx context.Context) (string, error)

	// Enumerate lists every message id in the account, page by page. cursor is
	// "" for the first page; next is "" after the last page. Envelopes carry
	// at least RemoteID and ThreadID; Flags/Mailboxes if cheap.
	Enumerate(ctx context.Context, cursor string, limit int) (page []Envelope, next string, err error)

	// FetchRaw fetches full messages for ids and calls fn for each one as it
	// arrives (order not guaranteed). fn is called serially — never from two
	// goroutines at once — so an implementation that fetches in parallel must
	// hold a lock around the call. fn returning an error aborts the fetch.
	// Ids that no longer exist on the server are silently skipped.
	FetchRaw(ctx context.Context, ids []string, fn func(RawMessage) error) error

	// Changes returns everything that changed since state. Returns
	// ErrStateExpired if the delta cannot be computed.
	Changes(ctx context.Context, since string) (*Changes, error)

	// --- writes ---------------------------------------------------------

	SetFlags(ctx context.Context, ids []string, set, clear model.Flags) error
	// SetMailboxes adds/removes the message from mailboxes (remote ids).
	SetMailboxes(ctx context.Context, ids []string, add, remove []string) error
	// Trash moves messages to the trash mailbox (not permanent delete).
	Trash(ctx context.Context, ids []string) error
	// Restore moves messages back to the inbox, out of the archive or trash.
	Restore(ctx context.Context, ids []string) error
	// CreateDraft stores raw as a draft and returns its remote id.
	CreateDraft(ctx context.Context, raw []byte) (remoteID string, err error)
	// Send submits raw. threadID (may be "") lets Gmail attach it to a thread.
	// Returns the remote id of the sent message when known.
	Send(ctx context.Context, raw []byte, threadID string) (remoteID string, err error)
	// FetchAttachment downloads a single attachment by provider reference
	// (used only when the raw message was not stored in full).
	FetchAttachment(ctx context.Context, messageID, ref string) ([]byte, error)
}

// EventChanges is the result of a calendar delta.
type EventChanges struct {
	// Upserted events (masters and exception instances). CalendarRemote and
	// RemoteID must be set; AccountID is filled by the caller.
	Upserted []model.Event
	Removed  []string // remote event ids
	NewState string
}

// CalendarProvider is implemented by every calendar backend.
type CalendarProvider interface {
	Calendars(ctx context.Context) ([]model.Calendar, error)
	// EventChanges returns changes for one calendar since state. since=="" means
	// a full listing. Returns ErrStateExpired when a full listing is needed.
	EventChanges(ctx context.Context, calendarRemote, since string) (*EventChanges, error)

	CreateEvent(ctx context.Context, calendarRemote string, ev *model.Event) (*model.Event, error)
	UpdateEvent(ctx context.Context, ev *model.Event) (*model.Event, error)
	DeleteEvent(ctx context.Context, calendarRemote, remoteID string) error
	// Respond sets the user's own participation status.
	Respond(ctx context.Context, calendarRemote, remoteID string, resp model.Participation) error
}

// Remapper is implemented by providers whose writes move a message's remote id.
//
// On IMAP a message is (folder, uidvalidity, uid), so COPY and MOVE mint a new
// uid for the copy — the id the caller passed in no longer names anything. The
// server reports the mapping (RFC 4315 COPYUID), so rather than let the row be
// deleted and re-fetched under a new id, the provider hands the mapping back and
// the engine renames the row in place, keeping its blob, thread and flags.
//
// The engine prefers these over the plain SetMailboxes/Trash when a provider
// implements them. Returning no renames is legal: a server without COPYUID
// leaves the delta to discover the move as a removal plus an addition.
type Remapper interface {
	SetMailboxesRemap(ctx context.Context, ids []string, add, remove []string) ([]Rename, error)
	TrashRemap(ctx context.Context, ids []string) ([]Rename, error)
	RestoreRemap(ctx context.Context, ids []string) ([]Rename, error)
}

// SubmitEnvelope carries the recipients a raw message deliberately does not.
//
// Bcc must not appear in the bytes that reach the recipients, so the built
// message omits the header entirely — which leaves an SMTP submission with
// nothing to put in RCPT TO. Providers that submit over SMTP need the envelope
// stated separately; API backends that take the whole message at once (Gmail,
// JMAP) read the recipients out of it themselves.
type SubmitEnvelope struct {
	// From is the envelope sender (SMTP MAIL FROM).
	From string
	// To is every recipient: To, Cc and Bcc together.
	To []string
	// ThreadID is what Send's own threadID argument carries: the thread to
	// attach the message to, where the backend has such a notion (Gmail). It
	// rides along here because Submit replaces Send entirely, so it has to
	// carry everything Send was given. SMTP has nowhere to put it.
	ThreadID string
}

// Submitter is implemented by backends that need an explicit envelope to send.
// The engine prefers it over MailProvider.Send when both are available.
type Submitter interface {
	Submit(ctx context.Context, raw []byte, env SubmitEnvelope) (remoteID string, err error)
}

// ChangeHint is delivered by a Pusher when the server signals a change.
type ChangeHint struct {
	Mail     bool
	Calendar bool
}

// Pusher is optionally implemented by providers that support server push
// (JMAP EventSource). Watch blocks until ctx is done; it must reconnect with
// backoff on transient failures and only return on ctx cancellation or a
// permanent error (e.g. auth).
type Pusher interface {
	Watch(ctx context.Context, fn func(ChangeHint)) error
}

// IsOffline reports whether err represents a transport-level failure
// (implementations wrap such errors with model.ErrOffline).
func IsOffline(err error) bool { return errors.Is(err, model.ErrOffline) }

// ErrNotConnected marks a failure that happened before any request bytes were
// sent: no network, no session, no client. Wrap it (with %w, alongside
// model.ErrOffline) whenever an implementation knows the request never left the
// machine — it is what lets the sync engine queue a non-idempotent write like a
// send instead of reporting it as possibly-half-done.
var ErrNotConnected = errors.New("not connected")

// IsPreRequestFailure reports whether err demonstrably happened before any
// request bytes reached the server, which makes retrying it safe even for a
// write that must not run twice.
//
// It is deliberately conservative: only a failure to establish the connection
// counts. A timeout, an EOF, a reset, a 5xx or a cancelled context can all mean
// the request arrived and the answer was lost, so they report false and the
// caller must treat the write as possibly-done.
//
// Note that this can only see what the error chain preserves. An
// implementation that formats its cause with %v instead of %w flattens the
// chain, and every failure from it looks ambiguous — which errs toward not
// retrying, never toward sending twice.
func IsPreRequestFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotConnected) {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	// *url.Error unwraps to whatever the transport returned, so a dial failure
	// behind an HTTP client is reached by the same As.
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH)
}
