// Package fake is an in-memory MailProvider/CalendarProvider/Pusher used by
// the sync-engine and CLI tests. It is a copy of the sync package's test fake
// with exported names.
package fake

import (
	"context"
	"fmt"
	"strconv"
	stdsync "sync"
	"time"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// ErrOffline is what the fake returns when FailNext is armed: a failure to
// reach the server at all, which provider.IsOffline recognises and
// provider.IsPreRequestFailure classifies as "the request never went out", so
// a queued write is safe to retry.
func ErrOffline(what string) error {
	return fmt.Errorf("fake: %s: dial tcp: %w: %w", what, provider.ErrNotConnected, model.ErrOffline)
}

// ErrOfflineAmbiguous is a transport failure that happened after the request
// was on the wire: the server may or may not have acted on it. It is offline
// but not a pre-request failure, so a non-idempotent write must not be retried.
func ErrOfflineAmbiguous(what string) error {
	return fmt.Errorf("fake: %s: read tcp: connection reset by peer: %w", what, model.ErrOffline)
}

// ---------------------------------------------------------------------------
// Mail

type Msg struct {
	id        string
	thread    string
	raw       []byte
	received  time.Time
	size      int64
	flags     model.Flags
	mailboxes []string

	// gone makes FetchRaw skip the id the way a provider does for a message
	// deleted between the enumeration and the fetch.
	gone bool
	// hidden keeps the message out of Enumerate (it still shows up in
	// Changes), which is how a message that arrives during a backfill behaves
	// when it lands on a page the cursor has already passed.
	hidden bool
}

type ChangeRec struct {
	state int
	id    string
	kind  deltaKind
}

// Mail is an in-memory MailProvider with knobs for everything the engine
// has to cope with.
type Mail struct {
	mu stdsync.Mutex

	boxes   []model.Mailbox
	msgs    map[string]*Msg
	order   []string
	state   int
	changes []ChangeRec

	pageSize int
	// expire arms the next Changes call to report an expired state.
	expire bool
	// failNext makes the next n provider calls fail as offline (pre-request).
	failNext int
	// failNextAmbiguous makes the next n calls fail after the request went out.
	failNextAmbiguous int
	// onEnumerate runs at the start of every Enumerate call (1-based count).
	onEnumerate func(call int)
	enumCalls   int

	mailboxesChanged bool
	// noTotal makes Total report that the provider cannot count.
	noTotal bool

	// recorded writes
	sentRaw   [][]byte
	draftRaw  [][]byte
	flagCalls int
	nextID    int

	// push
	pushMu  stdsync.Mutex
	pushFn  func(provider.ChangeHint)
	pushErr error
}

func NewMail() *Mail {
	return &Mail{
		msgs:     map[string]*Msg{},
		pageSize: 100,
		boxes: []model.Mailbox{
			{RemoteID: "INBOX", Name: "Inbox", Role: model.RoleInbox},
			{RemoteID: "ARCHIVE", Name: "Archive", Role: model.RoleArchive},
			{RemoteID: "TRASH", Name: "Trash", Role: model.RoleTrash},
			{RemoteID: "WORK", Name: "Work"},
		},
	}
}

// --- knobs -----------------------------------------------------------------

// Add stores a message and records it as an addition.
func (f *Mail) Add(m *Msg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addLocked(m)
}

func (f *Mail) addLocked(m *Msg) {
	if m.thread == "" {
		m.thread = m.id
	}
	if m.received.IsZero() {
		m.received = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	}
	if m.size == 0 {
		m.size = int64(len(m.raw))
	}
	if m.mailboxes == nil {
		m.mailboxes = []string{"INBOX"}
	}
	if _, ok := f.msgs[m.id]; !ok {
		f.order = append(f.order, m.id)
	}
	f.msgs[m.id] = m
	f.state++
	f.changes = append(f.changes, ChangeRec{state: f.state, id: m.id, kind: opAdded})
}

// Update changes flags/mailboxes and records an update.
func (f *Mail) Update(id string, flags model.Flags, mailboxes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.msgs[id]
	if !ok {
		return
	}
	m.flags = flags
	if mailboxes != nil {
		m.mailboxes = mailboxes
	}
	f.state++
	f.changes = append(f.changes, ChangeRec{state: f.state, id: id, kind: opUpdated})
}

// Remove deletes a message and records a removal.
func (f *Mail) Remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.msgs, id)
	for i, x := range f.order {
		if x == id {
			f.order = append(f.order[:i:i], f.order[i+1:]...)
			break
		}
	}
	f.state++
	f.changes = append(f.changes, ChangeRec{state: f.state, id: id, kind: opRemoved})
}

// AddMailbox appends a mailbox to the provider's list without telling the
// engine, so a message can reference one the index has never seen.
func (f *Mail) AddMailbox(mb model.Mailbox) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boxes = append(f.boxes, mb)
}

// MarkGone keeps the id in the listing but makes FetchRaw skip it.
func (f *Mail) MarkGone(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.msgs[id]; ok {
		m.gone = true
	}
}

// InjectStateExpired arms the next Changes call to fail with ErrStateExpired.
func (f *Mail) InjectStateExpired() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expire = true
}

// FailNext makes the next n provider calls fail as offline, before the request
// leaves the machine.
func (f *Mail) FailNext(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = n
}

// FailNextAmbiguous makes the next n provider calls fail the way a connection
// that drops mid-request does: the server may already have acted on it.
func (f *Mail) FailNextAmbiguous(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNextAmbiguous = n
}

func (f *Mail) SetPageSize(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pageSize = n
}

func (f *Mail) OnEnumerate(fn func(call int)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onEnumerate = fn
}

// Push delivers a hint to whoever is watching.
func (f *Mail) Push(h provider.ChangeHint) {
	f.pushMu.Lock()
	fn := f.pushFn
	f.pushMu.Unlock()
	if fn != nil {
		fn(h)
	}
}

func (f *Mail) fail() error {
	if f.failNext > 0 {
		f.failNext--
		return ErrOffline("mail")
	}
	if f.failNextAmbiguous > 0 {
		f.failNextAmbiguous--
		return ErrOfflineAmbiguous("mail")
	}
	return nil
}

// --- MailProvider ----------------------------------------------------------

func (f *Mail) Mailboxes(ctx context.Context) ([]model.Mailbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	out := make([]model.Mailbox, len(f.boxes))
	copy(out, f.boxes)
	return out, nil
}

func (f *Mail) State(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return "", err
	}
	return strconv.Itoa(f.state), nil
}

func (f *Mail) envelopeLocked(m *Msg) provider.Envelope {
	return provider.Envelope{
		RemoteID:  m.id,
		ThreadID:  m.thread,
		Received:  m.received,
		Size:      m.size,
		Flags:     m.flags,
		Mailboxes: append([]string(nil), m.mailboxes...),
	}
}

// Total implements the sync engine's optional total hint (the same shape as
// jmap.Totaler): how many messages the account holds. SetNoTotal switches it
// off, which is how a provider that cannot count behaves.
func (f *Mail) Total(ctx context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return 0, err
	}
	if f.noTotal {
		return 0, provider.ErrNotSupported
	}
	return len(f.order), nil
}

// SetNoTotal makes Total report that this provider cannot count messages.
func (f *Mail) SetNoTotal(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noTotal = v
}

func (f *Mail) Enumerate(ctx context.Context, cursor string, limit int) ([]provider.Envelope, string, error) {
	f.mu.Lock()
	f.enumCalls++
	hook, call := f.onEnumerate, f.enumCalls
	f.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, "", err
	}

	start := 0
	if cursor != "" {
		var err error
		start, err = strconv.Atoi(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("fake: bad cursor %q", cursor)
		}
	}
	n := f.pageSize
	if limit > 0 && limit < n {
		n = limit
	}
	var page []provider.Envelope
	i := start
	for ; i < len(f.order) && len(page) < n; i++ {
		m := f.msgs[f.order[i]]
		if m == nil || m.hidden {
			continue
		}
		page = append(page, f.envelopeLocked(m))
	}
	next := ""
	if i < len(f.order) {
		next = strconv.Itoa(i)
	}
	return page, next, nil
}

func (f *Mail) FetchRaw(ctx context.Context, ids []string, fn func(provider.RawMessage) error) error {
	f.mu.Lock()
	if err := f.fail(); err != nil {
		f.mu.Unlock()
		return err
	}
	var out []provider.RawMessage
	for _, id := range ids {
		m := f.msgs[id]
		if m == nil || m.gone {
			continue
		}
		out = append(out, provider.RawMessage{
			Envelope: f.envelopeLocked(m),
			Raw:      append([]byte(nil), m.raw...),
		})
	}
	f.mu.Unlock()

	for _, rm := range out {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(rm); err != nil {
			return err
		}
	}
	return nil
}

func (f *Mail) FetchEnvelopes(ctx context.Context, ids []string, fn func(provider.Envelope) error) error {
	f.mu.Lock()
	if err := f.fail(); err != nil {
		f.mu.Unlock()
		return err
	}
	var out []provider.Envelope
	for _, id := range ids {
		if m := f.msgs[id]; m != nil {
			out = append(out, f.envelopeLocked(m))
		}
	}
	f.mu.Unlock()

	for _, env := range out {
		if err := fn(env); err != nil {
			return err
		}
	}
	return nil
}

func (f *Mail) Changes(ctx context.Context, since string) (*provider.Changes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	if f.expire {
		f.expire = false
		return nil, provider.ErrStateExpired
	}
	from, err := strconv.Atoi(since)
	if err != nil {
		return nil, provider.ErrStateExpired
	}

	ch := &provider.Changes{NewState: strconv.Itoa(f.state), MailboxesChanged: f.mailboxesChanged}
	f.mailboxesChanged = false
	for _, c := range f.changes {
		if c.state <= from {
			continue
		}
		switch c.kind {
		case opAdded:
			if m := f.msgs[c.id]; m != nil {
				ch.Added = append(ch.Added, f.envelopeLocked(m))
			}
		case opUpdated:
			if m := f.msgs[c.id]; m != nil {
				ch.Updated = append(ch.Updated, f.envelopeLocked(m))
			}
		case opRemoved:
			ch.Removed = append(ch.Removed, c.id)
		}
	}
	return ch, nil
}

func (f *Mail) SetFlags(ctx context.Context, ids []string, set, clear model.Flags) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return err
	}
	f.flagCalls++
	for _, id := range ids {
		m := f.msgs[id]
		if m == nil {
			continue
		}
		m.flags = applyFlags(m.flags, set, clear)
		f.state++
		f.changes = append(f.changes, ChangeRec{state: f.state, id: id, kind: opUpdated})
	}
	return nil
}

func (f *Mail) SetMailboxes(ctx context.Context, ids []string, add, remove []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return err
	}
	for _, id := range ids {
		m := f.msgs[id]
		if m == nil {
			continue
		}
		next := withoutAll(append([]string(nil), m.mailboxes...), remove)
		for _, a := range add {
			if !contains(next, a) {
				next = append(next, a)
			}
		}
		m.mailboxes = next
		f.state++
		f.changes = append(f.changes, ChangeRec{state: f.state, id: id, kind: opUpdated})
	}
	return nil
}

func (f *Mail) Trash(ctx context.Context, ids []string) error {
	return f.SetMailboxes(ctx, ids, []string{"TRASH"}, []string{"INBOX", "ARCHIVE", "WORK"})
}

func (f *Mail) Restore(ctx context.Context, ids []string) error {
	return f.SetMailboxes(ctx, ids, []string{"INBOX"}, []string{"ARCHIVE", "TRASH"})
}

func (f *Mail) CreateDraft(ctx context.Context, raw []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return "", err
	}
	f.draftRaw = append(f.draftRaw, raw)
	f.nextID++
	return fmt.Sprintf("draft-%d", f.nextID), nil
}

func (f *Mail) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return "", err
	}
	f.sentRaw = append(f.sentRaw, raw)
	f.nextID++
	return fmt.Sprintf("sent-%d", f.nextID), nil
}

func (f *Mail) FetchAttachment(ctx context.Context, messageID, ref string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	return []byte("attachment:" + messageID + ":" + ref), nil
}

// --- Pusher ----------------------------------------------------------------

func (f *Mail) Watch(ctx context.Context, fn func(provider.ChangeHint)) error {
	f.pushMu.Lock()
	f.pushFn = fn
	err := f.pushErr
	f.pushMu.Unlock()
	if err != nil {
		return err
	}
	<-ctx.Done()
	f.pushMu.Lock()
	f.pushFn = nil
	f.pushMu.Unlock()
	return ctx.Err()
}

// BareMail is a MailProvider without FetchEnvelopes, to exercise the
// reconcile path that cannot refresh flags cheaply.
type BareMail struct{ f *Mail }

func (b BareMail) Mailboxes(ctx context.Context) ([]model.Mailbox, error) { return b.f.Mailboxes(ctx) }
func (b BareMail) State(ctx context.Context) (string, error)              { return b.f.State(ctx) }
func (b BareMail) Enumerate(ctx context.Context, cursor string, limit int) ([]provider.Envelope, string, error) {
	return b.f.Enumerate(ctx, cursor, limit)
}
func (b BareMail) FetchRaw(ctx context.Context, ids []string, fn func(provider.RawMessage) error) error {
	return b.f.FetchRaw(ctx, ids, fn)
}
func (b BareMail) Changes(ctx context.Context, since string) (*provider.Changes, error) {
	return b.f.Changes(ctx, since)
}
func (b BareMail) SetFlags(ctx context.Context, ids []string, set, clear model.Flags) error {
	return b.f.SetFlags(ctx, ids, set, clear)
}
func (b BareMail) SetMailboxes(ctx context.Context, ids []string, add, remove []string) error {
	return b.f.SetMailboxes(ctx, ids, add, remove)
}
func (b BareMail) Trash(ctx context.Context, ids []string) error { return b.f.Trash(ctx, ids) }
func (b BareMail) Restore(ctx context.Context, ids []string) error {
	return b.f.Restore(ctx, ids)
}
func (b BareMail) CreateDraft(ctx context.Context, raw []byte) (string, error) {
	return b.f.CreateDraft(ctx, raw)
}
func (b BareMail) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	return b.f.Send(ctx, raw, threadID)
}
func (b BareMail) FetchAttachment(ctx context.Context, messageID, ref string) ([]byte, error) {
	return b.f.FetchAttachment(ctx, messageID, ref)
}

// ---------------------------------------------------------------------------
// Calendar

type CalChange struct {
	state   int
	id      string
	removed bool
}

type Calendar struct {
	mu stdsync.Mutex

	cals    []model.Calendar
	events  map[string]map[string]*model.Event // calendar remote -> event remote
	state   map[string]int
	changes map[string][]CalChange
	expire  map[string]bool

	failNext int
	nextID   int
}

func NewCalendar() *Calendar {
	return &Calendar{
		cals:    []model.Calendar{{RemoteID: "primary", Name: "Primary", Primary: true, Timezone: "UTC"}},
		events:  map[string]map[string]*model.Event{},
		state:   map[string]int{},
		changes: map[string][]CalChange{},
		expire:  map[string]bool{},
	}
}

func (f *Calendar) fail() error {
	if f.failNext > 0 {
		f.failNext--
		return ErrOffline("calendar")
	}
	return nil
}

// Put stores an event and records the change.
func (f *Calendar) Put(calRemote string, ev model.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putLocked(calRemote, ev)
}

func (f *Calendar) putLocked(calRemote string, ev model.Event) {
	if f.events[calRemote] == nil {
		f.events[calRemote] = map[string]*model.Event{}
	}
	ev.CalendarRemote = calRemote
	f.events[calRemote][ev.RemoteID] = &ev
	f.state[calRemote]++
	f.changes[calRemote] = append(f.changes[calRemote], CalChange{state: f.state[calRemote], id: ev.RemoteID})
}

func (f *Calendar) Drop(calRemote, remoteID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.events[calRemote], remoteID)
	f.state[calRemote]++
	f.changes[calRemote] = append(f.changes[calRemote],
		CalChange{state: f.state[calRemote], id: remoteID, removed: true})
}

func (f *Calendar) InjectStateExpired(calRemote string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expire[calRemote] = true
}

func (f *Calendar) FailNext(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = n
}

func (f *Calendar) Calendars(ctx context.Context) ([]model.Calendar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	return append([]model.Calendar(nil), f.cals...), nil
}

func (f *Calendar) EventChanges(ctx context.Context, calendarRemote, since string) (*provider.EventChanges, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	if f.expire[calendarRemote] {
		f.expire[calendarRemote] = false
		return nil, provider.ErrStateExpired
	}
	out := &provider.EventChanges{NewState: strconv.Itoa(f.state[calendarRemote])}
	if since == "" {
		for _, ev := range f.events[calendarRemote] {
			out.Upserted = append(out.Upserted, *ev)
		}
		sortEvents(out.Upserted)
		return out, nil
	}
	from, err := strconv.Atoi(since)
	if err != nil {
		return nil, provider.ErrStateExpired
	}
	seen := map[string]bool{}
	for _, c := range f.changes[calendarRemote] {
		if c.state <= from || seen[c.id] {
			continue
		}
		seen[c.id] = true
		if c.removed {
			out.Removed = append(out.Removed, c.id)
			continue
		}
		if ev := f.events[calendarRemote][c.id]; ev != nil {
			out.Upserted = append(out.Upserted, *ev)
		}
	}
	sortEvents(out.Upserted)
	return out, nil
}

// sortEvents puts masters before their exceptions, which is the order the
// engine needs to apply them.
func sortEvents(evs []model.Event) {
	for i := 1; i < len(evs); i++ {
		for j := i; j > 0; j-- {
			a, b := evs[j-1], evs[j]
			if rank(a) <= rank(b) {
				break
			}
			evs[j-1], evs[j] = b, a
		}
	}
}

func rank(e model.Event) int {
	if e.RecurrenceID != "" {
		return 1
	}
	return 0
}

func (f *Calendar) CreateEvent(ctx context.Context, calendarRemote string, ev *model.Event) (*model.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	f.nextID++
	out := *ev
	out.RemoteID = fmt.Sprintf("ev-%d", f.nextID)
	out.CalendarRemote = calendarRemote
	if out.CreateConference {
		// Stand in for Google minting a Meet room on request.
		out.ConferenceURL = "https://meet.example/" + out.RemoteID
		out.CreateConference = false
	}
	f.putLocked(calendarRemote, out)
	return &out, nil
}

func (f *Calendar) UpdateEvent(ctx context.Context, ev *model.Event) (*model.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	out := *ev
	if out.CreateConference {
		out.ConferenceURL = "https://meet.example/" + out.RemoteID
		out.CreateConference = false
	}
	f.putLocked(ev.CalendarRemote, out)
	return &out, nil
}

func (f *Calendar) DeleteEvent(ctx context.Context, calendarRemote, remoteID string) error {
	f.mu.Lock()
	if err := f.fail(); err != nil {
		f.mu.Unlock()
		return err
	}
	delete(f.events[calendarRemote], remoteID)
	f.state[calendarRemote]++
	f.changes[calendarRemote] = append(f.changes[calendarRemote],
		CalChange{state: f.state[calendarRemote], id: remoteID, removed: true})
	f.mu.Unlock()
	return nil
}

func (f *Calendar) Respond(ctx context.Context, calendarRemote, remoteID string, resp model.Participation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return err
	}
	ev := f.events[calendarRemote][remoteID]
	if ev == nil {
		return fmt.Errorf("fake: no event %q", remoteID)
	}
	ev.MyResponse = resp
	f.state[calendarRemote]++
	f.changes[calendarRemote] = append(f.changes[calendarRemote],
		CalChange{state: f.state[calendarRemote], id: remoteID})
	return nil
}

// ---------------------------------------------------------------------------
// Factory

type fakeFactory struct {
	mail     provider.MailProvider
	cal      provider.CalendarProvider
	pusher   provider.Pusher
	hasPush  bool
	mailErr  error
	calErr   error
	pushErr  error
	mailUsed int
}

func (f *fakeFactory) Mail(ctx context.Context, acct config.Account) (provider.MailProvider, error) {
	if f.mailErr != nil {
		return nil, f.mailErr
	}
	f.mailUsed++
	return f.mail, nil
}

func (f *fakeFactory) Calendar(ctx context.Context, acct config.Account) (provider.CalendarProvider, error) {
	if f.calErr != nil {
		return nil, f.calErr
	}
	return f.cal, nil
}

func (f *fakeFactory) Pusher(ctx context.Context, acct config.Account) (provider.Pusher, bool, error) {
	if f.pushErr != nil {
		return nil, false, f.pushErr
	}
	return f.pusher, f.hasPush, nil
}

// --- helpers copied from the sync package so the fake is self-contained ----

type deltaKind byte

const (
	opNone deltaKind = iota
	opAdded
	opUpdated
	opRemoved
)

func applyFlags(cur, set, clear model.Flags) model.Flags {
	if set.Unread {
		cur.Unread = true
	}
	if set.Flagged {
		cur.Flagged = true
	}
	if set.Draft {
		cur.Draft = true
	}
	if set.Answered {
		cur.Answered = true
	}
	if clear.Unread {
		cur.Unread = false
	}
	if clear.Flagged {
		cur.Flagged = false
	}
	if clear.Draft {
		cur.Draft = false
	}
	if clear.Answered {
		cur.Answered = false
	}
	return cur
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func withoutAll(s, drop []string) []string {
	if len(drop) == 0 {
		return s
	}
	out := s[:0:0]
	for _, x := range s {
		if !contains(drop, x) {
			out = append(out, x)
		}
	}
	return out
}

// --- exported helpers for tests outside this package ----------------------

// NewMsg builds a message with sensible defaults (thread = id, mailbox INBOX,
// received 2026-03-01 12:00 UTC). Use the With* setters to adjust.
func NewMsg(id string, raw []byte) *Msg { return &Msg{id: id, raw: raw} }

func (m *Msg) WithThread(t string) *Msg        { m.thread = t; return m }
func (m *Msg) WithReceived(t time.Time) *Msg   { m.received = t; return m }
func (m *Msg) WithFlags(f model.Flags) *Msg    { m.flags = f; return m }
func (m *Msg) WithMailboxes(mb ...string) *Msg { m.mailboxes = mb; return m }
func (m *Msg) WithSize(n int64) *Msg           { m.size = n; return m }
func (m *Msg) ID() string                      { return m.id }

// Sent returns every raw message passed to Send, oldest first.
func (f *Mail) Sent() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.sentRaw...)
}

// Drafts returns every raw message passed to CreateDraft, oldest first.
func (f *Mail) Drafts() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.draftRaw...)
}

// Lookup returns the current server-side flags and mailboxes of a message.
func (f *Mail) Lookup(id string) (flags model.Flags, mailboxes []string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.msgs[id]
	if !ok {
		return model.Flags{}, nil, false
	}
	return m.flags, append([]string(nil), m.mailboxes...), true
}

// Events returns the current events of a calendar, for assertions.
func (f *Calendar) Events(calRemote string) []model.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.Event
	for _, ev := range f.events[calRemote] {
		out = append(out, *ev)
	}
	sortEvents(out)
	return out
}
