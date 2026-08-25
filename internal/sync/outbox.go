package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lennert/emlcal/internal/calendar"
	"github.com/lennert/emlcal/internal/config"
	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/provider"
	"github.com/lennert/emlcal/internal/store"
)

// OpKind identifies a queued write. The values are what lands in outbox.kind.
type OpKind string

const (
	OpFlags        OpKind = "flags"
	OpMailboxes    OpKind = "mailboxes"
	OpArchive      OpKind = "archive"
	OpTrash        OpKind = "trash"
	OpDraft        OpKind = "draft"
	OpSend         OpKind = "send"
	OpEventCreate  OpKind = "event.create"
	OpEventUpdate  OpKind = "event.update"
	OpEventDelete  OpKind = "event.delete"
	OpEventRespond OpKind = "event.respond"
)

// Op is one write to push to a provider. Only the fields relevant to Kind are
// used; the whole struct is what gets serialised into the outbox payload, so
// a queued write survives a restart.
type Op struct {
	Kind OpKind   `json:"kind"`
	IDs  []string `json:"ids,omitempty"` // message remote ids

	Flags struct {
		Set   model.Flags `json:"set"`
		Clear model.Flags `json:"clear"`
	} `json:"flags"`

	AddMailboxes    []string `json:"add_mailboxes,omitempty"`
	RemoveMailboxes []string `json:"remove_mailboxes,omitempty"`

	Raw      []byte `json:"raw,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`

	Event          *model.Event        `json:"event,omitempty"`
	CalendarRemote string              `json:"calendar_remote,omitempty"`
	Response       model.Participation `json:"response,omitempty"`
}

// ApplyResult reports what happened to a write.
type ApplyResult struct {
	// Queued is true when the provider was unreachable and the row is pending.
	Queued bool `json:"queued"`
	// OutboxID is the row that records the write.
	OutboxID int64 `json:"outbox_id"`
	// RemoteID is the provider id of a created object (draft, sent message,
	// event), when the write produced one.
	RemoteID string `json:"remote_id,omitempty"`
}

// OutboxReport summarises a RetryOutbox pass.
type OutboxReport struct {
	Pending   int           `json:"pending"`
	Attempted int           `json:"attempted"`
	Done      int           `json:"done"`
	Failed    int           `json:"failed"`
	Skipped   int           `json:"skipped"` // not due yet, or given up on
	Duration  time.Duration `json:"duration"`
}

// maxOutboxAttempts is where we stop retrying and leave the row for the user.
const maxOutboxAttempts = 10

// Apply records op in the outbox, patches the index optimistically and tries
// to execute it right away, all as described in DESIGN.md §7.4.
//
//   - success              → the row is marked done, Queued is false
//   - provider unreachable → the row stays pending, Queued is true
//   - provider rejects it  → the optimistic patch is rolled back, the row is
//     marked permanently failed (it is never retried) and the error returned
//
// The rollback matters because a rejected write changed nothing on the server:
// no later delta mentions the id, so without it the optimistic patch would
// survive every sync until a full reconcile.
func (e *Engine) Apply(ctx context.Context, account string, op Op) (*ApplyResult, error) {
	acct, ok := e.cfg.Account(account)
	if !ok {
		return nil, fmt.Errorf("sync: unknown account %q", account)
	}
	if err := op.validate(); err != nil {
		return nil, err
	}
	if err := e.ensureAccount(ctx, *acct); err != nil {
		return nil, err
	}

	payload, err := json.Marshal(op)
	if err != nil {
		return nil, fmt.Errorf("sync: marshal op: %w", err)
	}

	var id int64
	var undo *rollback
	err = e.st.Tx(ctx, func(tx *store.Tx) error {
		var err error
		id, err = tx.EnqueueOutbox(ctx, account, string(op.Kind), payload)
		if err != nil {
			return err
		}
		undo, err = e.patchLocal(ctx, tx, *acct, op)
		return err
	})
	if err != nil {
		return nil, err
	}

	res := &ApplyResult{OutboxID: id}
	remote, err := e.execute(ctx, *acct, op)
	switch {
	case err == nil:
		res.RemoteID = remote
		if err := e.st.MarkOutboxDone(ctx, id); err != nil {
			return res, err
		}
		if err := e.afterExecute(ctx, *acct, op, remote); err != nil {
			return res, err
		}
		return res, nil

	case retryable(op, err):
		res.Queued = true
		if err := e.st.MarkOutboxFailed(ctx, id, err.Error()); err != nil {
			return res, err
		}
		e.log.Warn("write queued (offline)", "account", account, "kind", op.Kind, "outbox", id)
		return res, nil

	default:
		// The write will never go through: put the index back the way it was
		// and stop the row from being retried.
		e.rollback(ctx, *acct, undo)
		e.failPermanently(ctx, id, err)
		return res, err
	}
}

// retryable reports whether a failed write may safely be executed again, which
// is the same question as whether the outbox may keep it pending.
//
// Anything the provider actively rejected is final — a retry produces the same
// rejection. A transport failure normally is not: composing offline and
// sending when the network comes back is the point of the outbox (DESIGN §12).
//
// Sends and drafts narrow that. They are not idempotent, and an offline error
// can mean either "nothing left this machine" or "the request went out and the
// reply was lost". Only the first is safe to repeat, so they stay pending just
// for a failure that provably happened before any request bytes were sent — no
// route, no DNS, refused connection, no client at all. An ambiguous failure
// (timeout, reset, EOF) retires the row with its error, and the user decides
// whether to send again, because the alternative is delivering the message
// twice.
func retryable(op Op, err error) bool {
	if op.Kind == OpSend || op.Kind == OpDraft {
		return provider.IsPreRequestFailure(err)
	}
	return provider.IsOffline(err)
}

// failPermanently records a rejection the outbox must not retry.
func (e *Engine) failPermanently(ctx context.Context, id int64, cause error) {
	if err := e.st.MarkOutboxPermanentlyFailed(ctx, id, cause.Error()); err != nil {
		e.log.Warn("outbox", "err", err)
	}
	e.forgetAttempt(id)
}

func (op *Op) validate() error {
	switch op.Kind {
	case OpFlags, OpMailboxes, OpArchive, OpTrash:
		if len(op.IDs) == 0 {
			return fmt.Errorf("sync: %s: no message ids", op.Kind)
		}
	case OpDraft, OpSend:
		if len(op.Raw) == 0 {
			return fmt.Errorf("sync: %s: no message body", op.Kind)
		}
	case OpEventCreate, OpEventUpdate:
		if op.Event == nil {
			return fmt.Errorf("sync: %s: no event", op.Kind)
		}
	case OpEventDelete, OpEventRespond:
		if op.Event == nil && op.CalendarRemote == "" {
			return fmt.Errorf("sync: %s: no event", op.Kind)
		}
	default:
		return fmt.Errorf("sync: unknown op kind %q", op.Kind)
	}
	return nil
}

// calendarRemote returns the calendar the op addresses.
func (op *Op) calendarRemote() string {
	if op.CalendarRemote != "" {
		return op.CalendarRemote
	}
	if op.Event != nil {
		return op.Event.CalendarRemote
	}
	return ""
}

// ---------------------------------------------------------------------------
// Optimistic local patch

// rollback is everything patchLocal overwrote, kept so a rejected write can be
// undone. It is deliberately a value the caller holds for the duration of one
// Apply rather than something persisted: a crash before the provider answers
// leaves the row pending, which is the case RetryOutbox handles.
type rollback struct {
	msgs   []msgState
	events []eventState
}

// msgState is one message row's pre-patch flags and mailbox membership.
type msgState struct {
	remote    string
	flags     model.Flags
	mailboxes []string
}

// eventState is one event row before the patch. prev is nil when there was no
// row at all, in which case undoing the patch means deleting what we created.
type eventState struct {
	calendarID int64
	calRemote  string
	remote     string
	prev       *model.Event
}

func (rb *rollback) empty() bool { return rb == nil || (len(rb.msgs) == 0 && len(rb.events) == 0) }

// patchLocal applies the write to the index before the provider has confirmed
// it, so the CLI shows the new state immediately. The next delta confirms.
// It returns the pre-patch state, which Apply restores when the provider
// rejects the write outright.
func (e *Engine) patchLocal(ctx context.Context, tx *store.Tx, acct config.Account, op Op) (*rollback, error) {
	rb := &rollback{}
	switch op.Kind {
	case OpDraft, OpSend:
		// Nothing local: the message does not exist in the index yet and the
		// delta that follows the submission indexes it properly.
		return rb, nil

	case OpFlags:
		for _, id := range op.IDs {
			msg, err := tx.GetMessage(ctx, acct.Name, id)
			if errors.Is(err, model.ErrNotFound) {
				continue
			}
			if err != nil {
				return rb, err
			}
			rb.note(msg)
			f := applyFlags(msg.Flags, op.Flags.Set, op.Flags.Clear)
			if err := tx.UpdateMessageState(ctx, acct.Name, id, f, nil); err != nil {
				return rb, err
			}
		}
		return rb, nil

	case OpMailboxes, OpArchive, OpTrash:
		add, remove, clearOthers, err := e.mailboxPatch(ctx, tx, acct, op)
		if err != nil {
			return rb, err
		}
		for _, id := range op.IDs {
			msg, err := tx.GetMessage(ctx, acct.Name, id)
			if errors.Is(err, model.ErrNotFound) {
				continue
			}
			if err != nil {
				return rb, err
			}
			rb.note(msg)
			var next []string
			if !clearOthers {
				next = append(next, msg.MailboxRemotes...)
			}
			next = withoutAll(next, remove)
			for _, a := range add {
				if !contains(next, a) {
					next = append(next, a)
				}
			}
			if err := tx.UpdateMessageState(ctx, acct.Name, id, msg.Flags, nonNil(next)); err != nil {
				return rb, err
			}
		}
		return rb, nil

	case OpEventCreate, OpEventUpdate, OpEventRespond, OpEventDelete:
		return rb, e.patchEvent(ctx, tx, acct, op, rb)
	}
	return rb, nil
}

// note records a message row's current state before it is overwritten.
func (rb *rollback) note(msg *model.Message) {
	rb.msgs = append(rb.msgs, msgState{
		remote:    msg.RemoteID,
		flags:     msg.Flags,
		mailboxes: nonNil(append([]string(nil), msg.MailboxRemotes...)),
	})
}

// noteEvent records an event row (or its absence) before it is overwritten.
// A missing calendar means there is nothing local to undo.
func (rb *rollback) noteEvent(ctx context.Context, tx *store.Tx, account, calRemote, remote string) error {
	if calRemote == "" || remote == "" {
		return nil
	}
	cal, err := tx.GetCalendarByRemote(ctx, account, calRemote)
	if errors.Is(err, model.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	st := eventState{calendarID: cal.ID, calRemote: calRemote, remote: remote}
	prev, err := tx.GetEvent(ctx, account, calRemote, remote)
	switch {
	case errors.Is(err, model.ErrNotFound):
		// st.prev stays nil: undoing means removing the row we are about to add.
	case err != nil:
		return err
	default:
		st.prev = prev
	}
	rb.events = append(rb.events, st)
	return nil
}

// rollback undoes the optimistic patch after a rejection. Failures here are
// logged rather than returned: the caller is already reporting the rejection,
// and a stale index entry is better reported than swallowed.
func (e *Engine) rollback(ctx context.Context, acct config.Account, rb *rollback) {
	if rb.empty() {
		return
	}
	err := e.st.Tx(ctx, func(tx *store.Tx) error {
		for _, m := range rb.msgs {
			err := tx.UpdateMessageState(ctx, acct.Name, m.remote, m.flags, m.mailboxes)
			if errors.Is(err, model.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
		}
		for _, ev := range rb.events {
			if ev.prev == nil {
				if err := tx.DeleteEventRow(ctx, ev.calendarID, ev.remote); err != nil {
					return err
				}
				continue
			}
			restored := *ev.prev
			if _, err := tx.UpsertEvent(ctx, &restored); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		e.log.Warn("could not undo the optimistic patch of a rejected write",
			"account", acct.Name, "err", err)
		return
	}
	// An event that was restored (or un-deleted) needs its occurrences back.
	for _, ev := range rb.events {
		if ev.prev == nil {
			continue
		}
		if err := e.expandEvent(ctx, acct, ev.calRemote, ev.remote); err != nil {
			e.log.Warn("could not re-expand a restored event",
				"account", acct.Name, "event", ev.remote, "err", err)
		}
	}
}

// mailboxPatch resolves an op into the mailbox remote ids to add and remove.
// clearOthers is set for a trash, where the message ends up in trash only.
func (e *Engine) mailboxPatch(ctx context.Context, tx *store.Tx, acct config.Account, op Op) (add, remove []string, clearOthers bool, err error) {
	switch op.Kind {
	case OpMailboxes:
		return op.AddMailboxes, op.RemoveMailboxes, false, nil

	case OpArchive:
		inbox, err := roleRemote(ctx, tx, acct.Name, model.RoleInbox)
		if err != nil {
			return nil, nil, false, err
		}
		if inbox != "" {
			remove = append(remove, inbox)
		}
		// Gmail archives by dropping INBOX and has no archive label; JMAP
		// wants the message filed somewhere, so move it to the archive folder.
		if archive, err := roleRemote(ctx, tx, acct.Name, model.RoleArchive); err != nil {
			return nil, nil, false, err
		} else if archive != "" {
			add = append(add, archive)
		}
		return add, remove, false, nil

	case OpTrash:
		trash, err := roleRemote(ctx, tx, acct.Name, model.RoleTrash)
		if err != nil {
			return nil, nil, false, err
		}
		if trash != "" {
			add = append(add, trash)
		}
		return add, nil, true, nil
	}
	return nil, nil, false, nil
}

func roleRemote(ctx context.Context, tx *store.Tx, account string, role model.MailboxRole) (string, error) {
	mbs, err := tx.ListMailboxes(ctx, account)
	if err != nil {
		return "", err
	}
	for _, m := range mbs {
		if m.Role == role {
			return m.RemoteID, nil
		}
	}
	return "", nil
}

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

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
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

// pendingPrefix marks the placeholder remote id an optimistically created
// event carries until the provider hands out the real one.
const pendingPrefix = "pending:"

func (e *Engine) patchEvent(ctx context.Context, tx *store.Tx, acct config.Account, op Op, rb *rollback) error {
	switch op.Kind {
	case OpEventCreate:
		ev := *op.Event
		ev.AccountID = acct.Name
		if ev.CalendarRemote == "" {
			ev.CalendarRemote = op.CalendarRemote
		}
		if ev.RemoteID == "" {
			ev.RemoteID = fmt.Sprintf("%s%d", pendingPrefix, time.Now().UnixNano())
			op.Event.RemoteID = ev.RemoteID
		}
		if err := rb.noteEvent(ctx, tx, acct.Name, ev.CalendarRemote, ev.RemoteID); err != nil {
			return err
		}
		_, err := tx.UpsertEvent(ctx, &ev)
		return err

	case OpEventUpdate:
		ev := *op.Event
		ev.AccountID = acct.Name
		if ev.CalendarRemote == "" {
			ev.CalendarRemote = op.CalendarRemote
		}
		if err := rb.noteEvent(ctx, tx, acct.Name, ev.CalendarRemote, ev.RemoteID); err != nil {
			return err
		}
		_, err := tx.UpsertEvent(ctx, &ev)
		return err

	case OpEventRespond:
		ev, err := tx.GetEvent(ctx, acct.Name, op.calendarRemote(), op.eventRemote())
		if errors.Is(err, model.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := rb.noteEvent(ctx, tx, acct.Name, op.calendarRemote(), op.eventRemote()); err != nil {
			return err
		}
		ev.MyResponse = op.Response
		for i := range ev.Attendees {
			if ev.Attendees[i].Self {
				ev.Attendees[i].Response = op.Response
			}
		}
		_, err = tx.UpsertEvent(ctx, ev)
		return err

	case OpEventDelete:
		cal, err := tx.GetCalendarByRemote(ctx, acct.Name, op.calendarRemote())
		if errors.Is(err, model.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := rb.noteEvent(ctx, tx, acct.Name, op.calendarRemote(), op.eventRemote()); err != nil {
			return err
		}
		return tx.MarkEventDeleted(ctx, cal.ID, op.eventRemote())
	}
	return nil
}

func (op *Op) eventRemote() string {
	if op.Event != nil {
		return op.Event.RemoteID
	}
	if len(op.IDs) > 0 {
		return op.IDs[0]
	}
	return ""
}

// ---------------------------------------------------------------------------
// Execution

// preRequest tags a *transport* failure to obtain a provider client. Nothing
// has been sent at that point — there is no connection yet — so even a send may
// be queued. Anything else (a missing token, a broken config) is not a
// connection problem and is left alone so it is treated as a rejection.
func preRequest(err error) error {
	if err == nil || !provider.IsOffline(err) {
		return err
	}
	return fmt.Errorf("%w: %w", provider.ErrNotConnected, err)
}

// execute pushes the op to the provider. It returns the remote id of anything
// the provider created.
func (e *Engine) execute(ctx context.Context, acct config.Account, op Op) (string, error) {
	switch op.Kind {
	case OpFlags, OpMailboxes, OpArchive, OpTrash, OpDraft, OpSend:
		mp, err := e.mailProvider(ctx, acct)
		if err != nil {
			return "", preRequest(err)
		}
		return e.executeMail(ctx, acct, mp, op)
	default:
		cp, err := e.calendarProvider(ctx, acct)
		if err != nil {
			return "", preRequest(err)
		}
		return e.executeEvent(ctx, acct, cp, op)
	}
}

func (e *Engine) executeMail(ctx context.Context, acct config.Account, mp provider.MailProvider, op Op) (string, error) {
	switch op.Kind {
	case OpFlags:
		return "", mp.SetFlags(ctx, op.IDs, op.Flags.Set, op.Flags.Clear)
	case OpMailboxes:
		return "", mp.SetMailboxes(ctx, op.IDs, op.AddMailboxes, op.RemoveMailboxes)
	case OpArchive:
		add, remove, _, err := e.resolveMailboxPatch(ctx, acct, op)
		if err != nil {
			return "", err
		}
		return "", mp.SetMailboxes(ctx, op.IDs, add, remove)
	case OpTrash:
		return "", mp.Trash(ctx, op.IDs)
	case OpDraft:
		return mp.CreateDraft(ctx, op.Raw)
	case OpSend:
		return mp.Send(ctx, op.Raw, op.ThreadID)
	}
	return "", fmt.Errorf("sync: unsupported mail op %q", op.Kind)
}

// resolveMailboxPatch is mailboxPatch outside a transaction.
func (e *Engine) resolveMailboxPatch(ctx context.Context, acct config.Account, op Op) (add, remove []string, clearOthers bool, err error) {
	err = e.st.Tx(ctx, func(tx *store.Tx) error {
		add, remove, clearOthers, err = e.mailboxPatch(ctx, tx, acct, op)
		return err
	})
	return add, remove, clearOthers, err
}

func (e *Engine) executeEvent(ctx context.Context, acct config.Account, cp provider.CalendarProvider, op Op) (string, error) {
	switch op.Kind {
	case OpEventCreate:
		ev := *op.Event
		if strings.HasPrefix(ev.RemoteID, pendingPrefix) {
			ev.RemoteID = ""
		}
		created, err := cp.CreateEvent(ctx, op.calendarRemote(), &ev)
		if err != nil {
			return "", err
		}
		if created != nil {
			return created.RemoteID, nil
		}
		return "", nil
	case OpEventUpdate:
		ev := *op.Event
		updated, err := cp.UpdateEvent(ctx, &ev)
		if err != nil {
			return "", err
		}
		if updated != nil {
			return updated.RemoteID, nil
		}
		return ev.RemoteID, nil
	case OpEventDelete:
		return "", cp.DeleteEvent(ctx, op.calendarRemote(), op.eventRemote())
	case OpEventRespond:
		return "", cp.Respond(ctx, op.calendarRemote(), op.eventRemote(), op.Response)
	}
	return "", fmt.Errorf("sync: unsupported calendar op %q", op.Kind)
}

// afterExecute reconciles the index with what the provider actually created.
func (e *Engine) afterExecute(ctx context.Context, acct config.Account, op Op, remote string) error {
	switch op.Kind {
	case OpEventCreate:
		if op.Event == nil {
			return nil
		}
		placeholder := op.Event.RemoteID
		if remote == "" || remote == placeholder {
			return e.expandEvent(ctx, acct, op.calendarRemote(), placeholder)
		}
		ev := *op.Event
		ev.AccountID = acct.Name
		ev.RemoteID = remote
		if ev.CalendarRemote == "" {
			ev.CalendarRemote = op.calendarRemote()
		}
		ev.ID = 0
		err := e.st.Tx(ctx, func(tx *store.Tx) error {
			if _, err := tx.UpsertEvent(ctx, &ev); err != nil {
				return err
			}
			if strings.HasPrefix(placeholder, pendingPrefix) {
				cal, err := tx.GetCalendarByRemote(ctx, acct.Name, ev.CalendarRemote)
				if err != nil {
					return err
				}
				return tx.MarkEventDeleted(ctx, cal.ID, placeholder)
			}
			return nil
		})
		if err != nil {
			return err
		}
		return e.expandEvent(ctx, acct, ev.CalendarRemote, remote)

	case OpEventUpdate, OpEventRespond:
		return e.expandEvent(ctx, acct, op.calendarRemote(), op.eventRemote())
	}
	return nil
}

// expandEvent re-materialises the occurrences of one event after a write.
func (e *Engine) expandEvent(ctx context.Context, acct config.Account, calRemote, remote string) error {
	if calRemote == "" || remote == "" {
		return nil
	}
	ev, err := e.st.GetEvent(ctx, acct.Name, calRemote, remote)
	if errors.Is(err, model.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	cal, err := e.st.GetCalendarByRemote(ctx, acct.Name, calRemote)
	if err != nil {
		return err
	}
	run := &calRun{e: e, acct: acct, cal: *cal}
	run.from, run.to = calendar.DefaultWindow(time.Now())
	if ev.RRule == "" && ev.RecurrenceID == "" {
		return run.expandSingle(ctx, ev)
	}
	if err := run.load(ctx); err != nil {
		return err
	}
	return run.expandSeries(ctx, ev.UID)
}

// ---------------------------------------------------------------------------
// Retry

// RetryOutbox executes pending writes oldest first, with an attempt-based
// backoff (1m·2^attempts, capped at an hour). account "" covers all accounts.
//
// A row leaves the queue in one of three ways: it goes through, it is given up
// on after maxOutboxAttempts, or the provider rejects it in a way that will
// never succeed. The last two both stamp failed_at, so the row stops being
// pending — nothing here re-sends a message the server already refused.
func (e *Engine) RetryOutbox(ctx context.Context, account string) (*OutboxReport, error) {
	started := time.Now()
	rep := &OutboxReport{}
	items, err := e.st.ListOutbox(ctx, true)
	if err != nil {
		return rep, err
	}
	pendingIDs := make(map[int64]bool, len(items))
	for _, it := range items {
		pendingIDs[it.ID] = true
	}
	defer e.pruneRetryAt(pendingIDs)

	var firstErr error
	for _, it := range items {
		if account != "" && it.AccountID != account {
			continue
		}
		rep.Pending++
		if it.Attempts >= maxOutboxAttempts {
			// A row from before failed_at existed, or one whose last attempt
			// exhausted the budget: retire it instead of listing it forever.
			e.failPermanently(ctx, it.ID, fmt.Errorf("gave up after %d attempts: %s", it.Attempts, it.LastError))
			rep.Skipped++
			continue
		}
		if !e.dueNow(it) {
			rep.Skipped++
			continue
		}
		acct, ok := e.cfg.Account(it.AccountID)
		if !ok {
			rep.Skipped++
			continue
		}
		var op Op
		if err := json.Unmarshal(it.Payload, &op); err != nil {
			// A payload that does not parse will not parse on the next pass
			// either; there is nothing to retry.
			rep.Failed++
			e.failPermanently(ctx, it.ID, fmt.Errorf("bad payload: %w", err))
			continue
		}

		rep.Attempted++
		remote, err := e.execute(ctx, *acct, op)
		e.noteAttempt(it.ID, it.Attempts+1)
		switch {
		case err == nil:
			if err := e.st.MarkOutboxDone(ctx, it.ID); err != nil {
				return rep, err
			}
			if err := e.afterExecute(ctx, *acct, op, remote); err != nil {
				e.log.Warn("outbox post-apply", "outbox", it.ID, "err", err)
			}
			e.forgetAttempt(it.ID)
			rep.Done++

		case !retryable(op, err):
			// Rejected outright, or a send whose fate we cannot know: stop.
			// Retiring a write is reported to the caller even when the cause
			// was a transport failure — the write is dead and the user has to
			// decide what to do about it, which a silent pass would hide.
			rep.Failed++
			e.failPermanently(ctx, it.ID, err)
			if firstErr == nil {
				firstErr = err
			}
			e.log.Warn("outbox write given up on",
				"account", it.AccountID, "kind", it.Kind, "outbox", it.ID, "err", err)

		case it.Attempts+1 >= maxOutboxAttempts:
			rep.Failed++
			e.failPermanently(ctx, it.ID, fmt.Errorf("gave up after %d attempts: %w", it.Attempts+1, err))
			e.log.Warn("outbox write given up on",
				"account", it.AccountID, "kind", it.Kind, "outbox", it.ID, "err", err)

		default:
			rep.Failed++
			if merr := e.st.MarkOutboxFailed(ctx, it.ID, err.Error()); merr != nil {
				return rep, merr
			}
			e.log.Warn("outbox retry failed",
				"account", it.AccountID, "kind", it.Kind, "outbox", it.ID, "err", err)
		}
		if err := ctx.Err(); err != nil {
			return rep, err
		}
	}
	rep.Duration = time.Since(started)
	if rep.Attempted > 0 {
		e.emit(ProgressEvent{
			Resource: "outbox", Phase: "outbox",
			Done: rep.Done, Total: rep.Attempted,
		})
	}
	return rep, firstErr
}

// dueNow reports whether an item's backoff has elapsed. The outbox table has
// no last-attempt column, so the timer lives in the engine: a fresh process
// retries everything once, which is exactly what a restart should do.
func (e *Engine) dueNow(it store.OutboxItem) bool {
	e.retryMu.Lock()
	defer e.retryMu.Unlock()
	at, ok := e.retryAt[it.ID]
	return !ok || !time.Now().Before(at)
}

func (e *Engine) noteAttempt(id int64, attempts int) {
	e.retryMu.Lock()
	defer e.retryMu.Unlock()
	e.retryAt[id] = time.Now().Add(backoff(attempts))
}

func (e *Engine) forgetAttempt(id int64) {
	e.retryMu.Lock()
	defer e.retryMu.Unlock()
	delete(e.retryAt, id)
}

// pruneRetryAt drops timers for rows that are no longer pending — retired,
// dropped by the user, or pruned — so the map cannot grow for the lifetime of
// a daemon.
func (e *Engine) pruneRetryAt(pending map[int64]bool) {
	e.retryMu.Lock()
	defer e.retryMu.Unlock()
	for id := range e.retryAt {
		if !pending[id] {
			delete(e.retryAt, id)
		}
	}
}

// backoff is 1m·2^(attempts-1), capped at an hour.
func backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := time.Minute
	for i := 1; i < attempts && d < time.Hour; i++ {
		d *= 2
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}
