package sync

import (
	"context"
	"fmt"
	"strconv"
	stdsync "sync"
	"time"

	"github.com/lennert/emlcal/internal/config"
	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/provider"
)

// errOffline is what the fake returns when FailNext is armed: a failure to
// reach the server at all, which provider.IsOffline recognises and
// provider.IsPreRequestFailure classifies as "the request never went out".
func errOffline(what string) error {
	return fmt.Errorf("fake: %s: dial tcp: %w: %w", what, provider.ErrNotConnected, model.ErrOffline)
}

// errOfflineAmbiguous is a transport failure from after the request was on the
// wire: offline, but the server may already have acted on it.
func errOfflineAmbiguous(what string) error {
	return fmt.Errorf("fake: %s: read tcp: connection reset by peer: %w", what, model.ErrOffline)
}

// ---------------------------------------------------------------------------
// Mail

type fakeMsg struct {
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

type changeRec struct {
	state int
	id    string
	kind  deltaKind
}

// fakeMail is an in-memory MailProvider with knobs for everything the engine
// has to cope with.
type fakeMail struct {
	mu stdsync.Mutex

	boxes   []model.Mailbox
	msgs    map[string]*fakeMsg
	order   []string
	state   int
	changes []changeRec

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

func newFakeMail() *fakeMail {
	return &fakeMail{
		msgs:     map[string]*fakeMsg{},
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
func (f *fakeMail) Add(m *fakeMsg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addLocked(m)
}

func (f *fakeMail) addLocked(m *fakeMsg) {
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
	f.changes = append(f.changes, changeRec{state: f.state, id: m.id, kind: opAdded})
}

// Update changes flags/mailboxes and records an update.
func (f *fakeMail) Update(id string, flags model.Flags, mailboxes []string) {
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
	f.changes = append(f.changes, changeRec{state: f.state, id: id, kind: opUpdated})
}

// Remove deletes a message and records a removal.
func (f *fakeMail) Remove(id string) {
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
	f.changes = append(f.changes, changeRec{state: f.state, id: id, kind: opRemoved})
}

// AddMailbox appends a mailbox to the provider's list without telling the
// engine, so a message can reference one the index has never seen.
func (f *fakeMail) AddMailbox(mb model.Mailbox) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boxes = append(f.boxes, mb)
}

// MarkGone keeps the id in the listing but makes FetchRaw skip it.
func (f *fakeMail) MarkGone(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.msgs[id]; ok {
		m.gone = true
	}
}

// InjectStateExpired arms the next Changes call to fail with ErrStateExpired.
func (f *fakeMail) InjectStateExpired() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expire = true
}

// FailNext makes the next n provider calls fail as offline, before the request
// leaves the machine.
func (f *fakeMail) FailNext(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = n
}

// FailNextAmbiguous makes the next n provider calls fail the way a connection
// that drops mid-request does: the server may already have acted on it.
func (f *fakeMail) FailNextAmbiguous(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNextAmbiguous = n
}

func (f *fakeMail) SetPageSize(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pageSize = n
}

func (f *fakeMail) OnEnumerate(fn func(call int)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onEnumerate = fn
}

// Push delivers a hint to whoever is watching.
func (f *fakeMail) Push(h provider.ChangeHint) {
	f.pushMu.Lock()
	fn := f.pushFn
	f.pushMu.Unlock()
	if fn != nil {
		fn(h)
	}
}

func (f *fakeMail) fail() error {
	if f.failNext > 0 {
		f.failNext--
		return errOffline("mail")
	}
	if f.failNextAmbiguous > 0 {
		f.failNextAmbiguous--
		return errOfflineAmbiguous("mail")
	}
	return nil
}

// --- MailProvider ----------------------------------------------------------

func (f *fakeMail) Mailboxes(ctx context.Context) ([]model.Mailbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	out := make([]model.Mailbox, len(f.boxes))
	copy(out, f.boxes)
	return out, nil
}

func (f *fakeMail) State(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return "", err
	}
	return strconv.Itoa(f.state), nil
}

func (f *fakeMail) envelopeLocked(m *fakeMsg) provider.Envelope {
	return provider.Envelope{
		RemoteID:  m.id,
		ThreadID:  m.thread,
		Received:  m.received,
		Size:      m.size,
		Flags:     m.flags,
		Mailboxes: append([]string(nil), m.mailboxes...),
	}
}

func (f *fakeMail) Enumerate(ctx context.Context, cursor string, limit int) ([]provider.Envelope, string, error) {
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

func (f *fakeMail) FetchRaw(ctx context.Context, ids []string, fn func(provider.RawMessage) error) error {
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

func (f *fakeMail) FetchEnvelopes(ctx context.Context, ids []string, fn func(provider.Envelope) error) error {
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

func (f *fakeMail) Changes(ctx context.Context, since string) (*provider.Changes, error) {
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

func (f *fakeMail) SetFlags(ctx context.Context, ids []string, set, clear model.Flags) error {
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
		f.changes = append(f.changes, changeRec{state: f.state, id: id, kind: opUpdated})
	}
	return nil
}

func (f *fakeMail) SetMailboxes(ctx context.Context, ids []string, add, remove []string) error {
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
		f.changes = append(f.changes, changeRec{state: f.state, id: id, kind: opUpdated})
	}
	return nil
}

func (f *fakeMail) Trash(ctx context.Context, ids []string) error {
	return f.SetMailboxes(ctx, ids, []string{"TRASH"}, []string{"INBOX", "ARCHIVE", "WORK"})
}

func (f *fakeMail) CreateDraft(ctx context.Context, raw []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return "", err
	}
	f.draftRaw = append(f.draftRaw, raw)
	f.nextID++
	return fmt.Sprintf("draft-%d", f.nextID), nil
}

func (f *fakeMail) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return "", err
	}
	f.sentRaw = append(f.sentRaw, raw)
	f.nextID++
	return fmt.Sprintf("sent-%d", f.nextID), nil
}

func (f *fakeMail) FetchAttachment(ctx context.Context, messageID, ref string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	return []byte("attachment:" + messageID + ":" + ref), nil
}

// --- Pusher ----------------------------------------------------------------

func (f *fakeMail) Watch(ctx context.Context, fn func(provider.ChangeHint)) error {
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

// bareMail is a MailProvider without FetchEnvelopes, to exercise the
// reconcile path that cannot refresh flags cheaply.
type bareMail struct{ f *fakeMail }

func (b bareMail) Mailboxes(ctx context.Context) ([]model.Mailbox, error) { return b.f.Mailboxes(ctx) }
func (b bareMail) State(ctx context.Context) (string, error)              { return b.f.State(ctx) }
func (b bareMail) Enumerate(ctx context.Context, cursor string, limit int) ([]provider.Envelope, string, error) {
	return b.f.Enumerate(ctx, cursor, limit)
}
func (b bareMail) FetchRaw(ctx context.Context, ids []string, fn func(provider.RawMessage) error) error {
	return b.f.FetchRaw(ctx, ids, fn)
}
func (b bareMail) Changes(ctx context.Context, since string) (*provider.Changes, error) {
	return b.f.Changes(ctx, since)
}
func (b bareMail) SetFlags(ctx context.Context, ids []string, set, clear model.Flags) error {
	return b.f.SetFlags(ctx, ids, set, clear)
}
func (b bareMail) SetMailboxes(ctx context.Context, ids []string, add, remove []string) error {
	return b.f.SetMailboxes(ctx, ids, add, remove)
}
func (b bareMail) Trash(ctx context.Context, ids []string) error { return b.f.Trash(ctx, ids) }
func (b bareMail) CreateDraft(ctx context.Context, raw []byte) (string, error) {
	return b.f.CreateDraft(ctx, raw)
}
func (b bareMail) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	return b.f.Send(ctx, raw, threadID)
}
func (b bareMail) FetchAttachment(ctx context.Context, messageID, ref string) ([]byte, error) {
	return b.f.FetchAttachment(ctx, messageID, ref)
}

// ---------------------------------------------------------------------------
// Calendar

type calChange struct {
	state   int
	id      string
	removed bool
}

type fakeCalendar struct {
	mu stdsync.Mutex

	cals    []model.Calendar
	events  map[string]map[string]*model.Event // calendar remote -> event remote
	state   map[string]int
	changes map[string][]calChange
	expire  map[string]bool

	failNext int
	nextID   int
}

func newFakeCalendar() *fakeCalendar {
	return &fakeCalendar{
		cals:    []model.Calendar{{RemoteID: "primary", Name: "Primary", Primary: true, Timezone: "UTC"}},
		events:  map[string]map[string]*model.Event{},
		state:   map[string]int{},
		changes: map[string][]calChange{},
		expire:  map[string]bool{},
	}
}

func (f *fakeCalendar) fail() error {
	if f.failNext > 0 {
		f.failNext--
		return errOffline("calendar")
	}
	return nil
}

// Put stores an event and records the change.
func (f *fakeCalendar) Put(calRemote string, ev model.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putLocked(calRemote, ev)
}

func (f *fakeCalendar) putLocked(calRemote string, ev model.Event) {
	if f.events[calRemote] == nil {
		f.events[calRemote] = map[string]*model.Event{}
	}
	ev.CalendarRemote = calRemote
	f.events[calRemote][ev.RemoteID] = &ev
	f.state[calRemote]++
	f.changes[calRemote] = append(f.changes[calRemote], calChange{state: f.state[calRemote], id: ev.RemoteID})
}

func (f *fakeCalendar) Drop(calRemote, remoteID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.events[calRemote], remoteID)
	f.state[calRemote]++
	f.changes[calRemote] = append(f.changes[calRemote],
		calChange{state: f.state[calRemote], id: remoteID, removed: true})
}

func (f *fakeCalendar) InjectStateExpired(calRemote string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expire[calRemote] = true
}

func (f *fakeCalendar) FailNext(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = n
}

func (f *fakeCalendar) Calendars(ctx context.Context) ([]model.Calendar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	return append([]model.Calendar(nil), f.cals...), nil
}

func (f *fakeCalendar) EventChanges(ctx context.Context, calendarRemote, since string) (*provider.EventChanges, error) {
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

func (f *fakeCalendar) CreateEvent(ctx context.Context, calendarRemote string, ev *model.Event) (*model.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	f.nextID++
	out := *ev
	out.RemoteID = fmt.Sprintf("ev-%d", f.nextID)
	out.CalendarRemote = calendarRemote
	f.putLocked(calendarRemote, out)
	return &out, nil
}

func (f *fakeCalendar) UpdateEvent(ctx context.Context, ev *model.Event) (*model.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	out := *ev
	f.putLocked(ev.CalendarRemote, out)
	return &out, nil
}

func (f *fakeCalendar) DeleteEvent(ctx context.Context, calendarRemote, remoteID string) error {
	f.mu.Lock()
	if err := f.fail(); err != nil {
		f.mu.Unlock()
		return err
	}
	delete(f.events[calendarRemote], remoteID)
	f.state[calendarRemote]++
	f.changes[calendarRemote] = append(f.changes[calendarRemote],
		calChange{state: f.state[calendarRemote], id: remoteID, removed: true})
	f.mu.Unlock()
	return nil
}

func (f *fakeCalendar) Respond(ctx context.Context, calendarRemote, remoteID string, resp model.Participation) error {
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
		calChange{state: f.state[calendarRemote], id: remoteID})
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
