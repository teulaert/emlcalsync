package sync

import (
	"context"
	"errors"
	"fmt"
	stdsync "sync"
	"time"

	"github.com/lennert/emlcal/internal/config"
	"github.com/lennert/emlcal/internal/mime"
	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/provider"
	"github.com/lennert/emlcal/internal/store"
)

// resourceMail is the sync_state / backfill_progress resource name for mail.
const resourceMail = "mail"

// Kinds reported in ResourceReport.Kind and sync_log.kind.
const (
	KindBackfill  = "backfill"
	KindResume    = "resume"
	KindDelta     = "delta"
	KindReconcile = "reconcile"
	KindCalendar  = "calendar"
)

// envelopeFetcher is the optional cheap-refresh interface some providers
// implement (gmail.Mail does): current flags/mailboxes without the raw body.
type envelopeFetcher interface {
	FetchEnvelopes(ctx context.Context, ids []string, fn func(provider.Envelope) error) error
}

// syncMail picks the right mail path for the account's current state:
// backfill on first run, resume when one was interrupted, reconcile on --full
// (or an expired state), delta otherwise.
func (e *Engine) syncMail(ctx context.Context, acct config.Account, full bool) (*ResourceReport, error) {
	mp, err := e.mailProvider(ctx, acct)
	if err != nil {
		return nil, err
	}
	r := &mailRun{
		e:          e,
		acct:       acct,
		mp:         mp,
		rawMax:     acct.EffectiveRawMaxSize(e.cfg.General).Bytes(),
		unresolved: map[string]bool{},
	}
	if err := r.loadMailboxes(ctx); err != nil {
		return nil, err
	}

	state, err := e.st.GetState(ctx, acct.Name, resourceMail)
	if err != nil {
		return nil, err
	}
	bf, err := e.st.GetBackfill(ctx, acct.Name, resourceMail)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}

	switch {
	case full && state != "":
		return r.reconcile(ctx, state)
	case full:
		if err := e.st.ClearBackfill(ctx, acct.Name, resourceMail); err != nil {
			return nil, err
		}
		return r.backfill(ctx, nil)
	case bf != nil && !bf.Finished():
		return r.backfill(ctx, bf)
	case state == "":
		return r.backfill(ctx, nil)
	default:
		return r.delta(ctx, state)
	}
}

// ---------------------------------------------------------------------------
// mailRun is the state of one mail sync pass.

type mailRun struct {
	e    *Engine
	acct config.Account
	mp   provider.MailProvider

	// rawMax is the effective raw_max_size in bytes; 0 = unlimited.
	rawMax int64

	// mbMu guards known/roles/unresolved and serialises the mailbox refresh.
	// FetchRaw promises to call its callback serially, but the callback path
	// reaches these maps and the engine should not corrupt its own state if a
	// provider ever breaks that promise.
	mbMu stdsync.Mutex
	// known is the set of mailbox remote ids the index knows about, so an
	// envelope naming an unfamiliar one can trigger a mailbox re-sync.
	known map[string]bool
	// roles maps a mailbox role to its remote id (first match wins).
	roles map[model.MailboxRole]string
	// unresolved remembers remote ids that stayed unknown after a refresh, so
	// one permanently odd id cannot make every batch re-fetch the mailboxes.
	unresolved map[string]bool
}

func (r *mailRun) account() string { return r.acct.Name }

// loadMailboxes fills known/roles from the index without touching the provider.
func (r *mailRun) loadMailboxes(ctx context.Context) error {
	mbs, err := r.e.st.ListMailboxes(ctx, r.account())
	if err != nil {
		return err
	}
	r.setMailboxes(mbs)
	return nil
}

// syncMailboxes re-fetches the mailbox list from the provider and stores it.
func (r *mailRun) syncMailboxes(ctx context.Context) error {
	r.mbMu.Lock()
	defer r.mbMu.Unlock()
	return r.syncMailboxesLocked(ctx)
}

func (r *mailRun) syncMailboxesLocked(ctx context.Context) error {
	mbs, err := r.mp.Mailboxes(ctx)
	if err != nil {
		return fmt.Errorf("sync: %s: mailboxes: %w", r.account(), err)
	}
	if err := r.e.st.ReplaceMailboxes(ctx, r.account(), mbs); err != nil {
		if errors.Is(err, store.ErrEmptyMailboxList) {
			// An empty-but-not-an-error response would delete every mailbox and
			// with it every message's membership. Keep what we have; the next
			// pass asks again.
			r.e.log.Warn("provider returned no mailboxes at all; keeping the stored list",
				"account", r.account())
			return nil
		}
		return err
	}
	r.setMailboxesLocked(mbs)
	return nil
}

func (r *mailRun) setMailboxes(mbs []model.Mailbox) {
	r.mbMu.Lock()
	defer r.mbMu.Unlock()
	r.setMailboxesLocked(mbs)
}

func (r *mailRun) setMailboxesLocked(mbs []model.Mailbox) {
	known := make(map[string]bool, len(mbs))
	roles := make(map[model.MailboxRole]string, len(mbs))
	for _, m := range mbs {
		known[m.RemoteID] = true
		if m.Role != "" {
			if _, ok := roles[m.Role]; !ok {
				roles[m.Role] = m.RemoteID
			}
		}
	}
	r.known, r.roles = known, roles
}

// noteUnknown re-syncs mailboxes when a message references a mailbox remote id
// the index has never seen: UpsertMessage silently drops such a membership, so
// the list has to be refreshed before the write, not after. Each unknown id
// triggers at most one refresh — one permanently odd id cannot make every
// batch re-fetch the mailbox list.
//
// The lock is held across the refresh, so two callers that both see the same
// unknown id do one refresh between them, not two.
func (r *mailRun) noteUnknown(ctx context.Context, groups ...[]string) {
	r.mbMu.Lock()
	defer r.mbMu.Unlock()

	fresh := false
	for _, g := range groups {
		for _, mb := range g {
			if !r.known[mb] && !r.unresolved[mb] {
				fresh = true
			}
		}
	}
	if !fresh {
		return
	}
	if err := r.syncMailboxesLocked(ctx); err != nil {
		r.e.log.Warn("mailbox refresh failed", "account", r.account(), "err", err)
		return
	}
	for _, g := range groups {
		for _, mb := range g {
			if !r.known[mb] {
				r.unresolved[mb] = true
			}
		}
	}
}

// mailboxesOf collects the mailbox lists of a batch for noteUnknown.
func mailboxesOf(batch []*pendingMsg) [][]string {
	out := make([][]string, 0, len(batch))
	for _, p := range batch {
		out = append(out, p.msg.MailboxRemotes)
	}
	return out
}

// ---------------------------------------------------------------------------
// Indexing

// pendingMsg is one message ready to be written: the blob is already on disk.
type pendingMsg struct {
	msg    *model.Message
	parsed *mime.Parsed
}

// prepare stores the raw bytes and parses them. The blob is written before the
// row on purpose: a crash can leave an orphan blob (which `gc` collects) but
// never a row pointing at a blob that is not there.
func (r *mailRun) prepare(rm provider.RawMessage) (*pendingMsg, error) {
	sha, _, err := r.e.blobs.Put(rm.Raw)
	if err != nil {
		return nil, fmt.Errorf("sync: %s: blob %s: %w", r.account(), rm.RemoteID, err)
	}
	parsed, perr := mime.Parse(rm.Raw)
	if perr != nil {
		r.e.log.Warn("mime parse failed", "account", r.account(), "message", rm.RemoteID, "err", perr)
		parsed = nil
	}
	msg := &model.Message{
		AccountID:      r.account(),
		RemoteID:       rm.RemoteID,
		ThreadID:       rm.ThreadID,
		BlobSHA256:     sha,
		RawComplete:    true,
		Received:       rm.Received,
		Size:           rm.Size,
		Flags:          rm.Flags,
		MailboxRemotes: nonNil(rm.Mailboxes),
		IndexedAt:      time.Now(),
	}
	if msg.Size == 0 {
		msg.Size = int64(len(rm.Raw))
	}
	if parsed != nil {
		msg.Snippet = mime.Snippet(parsed.TextBody, 200)
		msg.Date = parsed.Date
	}
	if msg.Date.IsZero() {
		msg.Date = rm.Received
	}
	return &pendingMsg{msg: msg, parsed: parsed}, nil
}

// stub builds the envelope-only row for a message we deliberately did not
// fetch because it is larger than raw_max_size. EnsureRaw completes it later.
func (r *mailRun) stub(env provider.Envelope) *pendingMsg {
	msg := &model.Message{
		AccountID:      r.account(),
		RemoteID:       env.RemoteID,
		ThreadID:       env.ThreadID,
		RawComplete:    false,
		Subject:        fmt.Sprintf("(not fetched: %s)", megabytes(env.Size)),
		Received:       env.Received,
		Date:           env.Received,
		Size:           env.Size,
		Flags:          env.Flags,
		MailboxRemotes: nonNil(env.Mailboxes),
		IndexedAt:      time.Now(),
	}
	return &pendingMsg{msg: msg}
}

// megabytes renders a size the way the stub subject spells it.
func megabytes(n int64) string {
	if n < 1000*1000 {
		return fmt.Sprintf("%d KB", (n+999)/1000)
	}
	return fmt.Sprintf("%d MB", (n+999_999)/1_000_000)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// writeBatch stores a batch of prepared messages, plus optional extra work, in
// one transaction.
func (r *mailRun) writeBatch(ctx context.Context, batch []*pendingMsg, tail func(*store.Tx) error) error {
	if len(batch) == 0 && tail == nil {
		return nil
	}
	r.noteUnknown(ctx, mailboxesOf(batch)...)
	return r.e.st.Tx(ctx, func(tx *store.Tx) error {
		for _, p := range batch {
			if _, err := tx.UpsertMessage(ctx, p.msg, p.parsed); err != nil {
				return err
			}
		}
		if tail != nil {
			return tail(tx)
		}
		return nil
	})
}

// resurrect brings back messages the index has marked deleted but the provider
// still lists. Clearing deleted_at alone is not enough — MarkDeleted also
// cleared the mailbox membership, and a message in no mailbox is invisible in
// every listing — so the envelope's mailbox list is applied at the same time.
// Envelopes without one (an oversize message whose provider gives no cheap
// membership) are un-deleted anyway and left for the next envelope refresh.
func (r *mailRun) resurrect(ctx context.Context, envs []provider.Envelope) error {
	if len(envs) == 0 {
		return nil
	}
	var unfiled []string
	for _, part := range chunk(envs, indexBatch) {
		groups := make([][]string, 0, len(part))
		for _, env := range part {
			groups = append(groups, env.Mailboxes)
		}
		r.noteUnknown(ctx, groups...)

		part := part
		err := r.e.st.Tx(ctx, func(tx *store.Tx) error {
			for _, env := range part {
				if len(env.Mailboxes) == 0 {
					unfiled = append(unfiled, env.RemoteID)
					continue
				}
				err := tx.UndeleteWithState(ctx, r.account(), env.RemoteID, env.Flags, nonNil(env.Mailboxes))
				if errors.Is(err, model.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(unfiled) > 0 {
		if _, err := r.e.st.MarkUndeleted(ctx, r.account(), unfiled); err != nil {
			return err
		}
		r.e.log.Warn("resurrected messages the provider gave no mailbox list for; they stay unfiled until the next refresh",
			"account", r.account(), "messages", len(unfiled))
	}
	return nil
}

// fetchIndex fetches ids from the provider and indexes them in transactions of
// indexBatch messages. tail runs inside the same transaction as the final
// batch, which is how the delta state is advanced atomically with the work it
// covers. Ids the provider no longer has come back in gone.
func (r *mailRun) fetchIndex(ctx context.Context, ids []string, phase string, done *int, tail func(*store.Tx) error) (gone []string, err error) {
	if len(ids) == 0 {
		if tail != nil {
			return nil, r.e.st.Tx(ctx, tail)
		}
		return nil, nil
	}

	var mu stdsync.Mutex
	batch := make([]*pendingMsg, 0, indexBatch)
	seen := make(map[string]bool, len(ids))

	flush := func(tail func(*store.Tx) error) error {
		mu.Lock()
		b := batch
		batch = make([]*pendingMsg, 0, indexBatch)
		mu.Unlock()
		if len(b) == 0 && tail == nil {
			return nil
		}
		if err := r.writeBatch(ctx, b, tail); err != nil {
			return err
		}
		if done != nil {
			// flush runs on the FetchRaw callback's goroutine, so the counter
			// goes under the same lock as the batch it belongs to.
			mu.Lock()
			*done += len(b)
			n := *done
			mu.Unlock()
			r.e.emit(ProgressEvent{
				Account: r.account(), Resource: resourceMail, Phase: phase,
				Done: n, Total: len(ids),
			})
		}
		return nil
	}

	ferr := r.mp.FetchRaw(ctx, ids, func(rm provider.RawMessage) error {
		p, err := r.prepare(rm)
		if err != nil {
			return err
		}
		mu.Lock()
		seen[rm.RemoteID] = true
		batch = append(batch, p)
		full := len(batch) >= indexBatch
		mu.Unlock()
		if full {
			return flush(nil)
		}
		return nil
	})
	if ferr != nil {
		// Whatever made it into the index before the failure stays there; the
		// state is not advanced, so the next pass replays the rest.
		return nil, fmt.Errorf("sync: %s: fetch: %w", r.account(), ferr)
	}
	if err := flush(tail); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	return gone, nil
}

// ---------------------------------------------------------------------------
// Backfill (DESIGN.md §7.1)

// backfill walks every message in the account. bf is a previously interrupted
// run to resume, or nil to start one.
func (r *mailRun) backfill(ctx context.Context, bf *store.Backfill) (*ResourceReport, error) {
	started := time.Now()
	kind := KindBackfill
	if bf != nil {
		kind = KindResume
	}
	rep := &ResourceReport{Kind: kind}

	if bf == nil {
		// The state token must be captured *before* enumeration so nothing
		// that happens during a multi-hour backfill is lost.
		s0, err := r.mp.State(ctx)
		if err != nil {
			return rep, fmt.Errorf("sync: %s: state: %w", r.account(), err)
		}
		bf = &store.Backfill{AccountID: r.account(), Resource: resourceMail, StateAtStart: s0}
		if err := r.e.st.SetBackfill(ctx, bf); err != nil {
			return rep, err
		}
	}
	rep.StateBefore = bf.StateAtStart

	if err := r.syncMailboxes(ctx); err != nil {
		return rep, err
	}

	done := bf.Done
	cursor := bf.Cursor
	for {
		page, next, err := r.mp.Enumerate(ctx, cursor, enumeratePage)
		if err != nil {
			r.finish(ctx, kind, started, rep, err)
			return rep, fmt.Errorf("sync: %s: enumerate: %w", r.account(), err)
		}

		var fetch []string
		var stubs []*pendingMsg
		var resurrect []provider.Envelope
		for _, env := range page {
			st, err := r.e.st.MessageIndexState(ctx, r.account(), env.RemoteID)
			if err != nil {
				return rep, err
			}
			oversize := r.rawMax > 0 && env.Size > r.rawMax
			if st.Exists && st.Deleted {
				// The provider still lists a message we marked deleted. Getting
				// deleted_at cleared is not enough: MarkDeleted also cleared the
				// mailbox membership, so it has to be rebuilt or the message
				// stays invisible in every listing.
				if len(env.Mailboxes) > 0 || oversize {
					resurrect = append(resurrect, env)
				} else {
					fetch = append(fetch, env.RemoteID) // the upsert un-deletes
				}
				continue
			}
			if st.Exists && st.RawComplete {
				continue
			}
			if oversize {
				if !st.Exists {
					stubs = append(stubs, r.stub(env))
				}
				continue
			}
			fetch = append(fetch, env.RemoteID)
		}

		if err := r.resurrect(ctx, resurrect); err != nil {
			return rep, err
		}

		for _, b := range chunk(stubs, indexBatch) {
			if err := r.writeBatch(ctx, b, nil); err != nil {
				return rep, err
			}
			rep.Added += len(b)
		}

		added := 0
		gone, err := r.fetchIndex(ctx, fetch, kind, &added, nil)
		rep.Added += added
		if err != nil {
			r.finish(ctx, kind, started, rep, err)
			return rep, err
		}
		if len(gone) > 0 {
			n, err := r.e.st.MarkDeleted(ctx, r.account(), gone)
			if err != nil {
				return rep, err
			}
			rep.Removed += n
		}

		done += len(page)
		bf.Cursor, bf.Done = next, done
		if err := r.e.st.SetBackfill(ctx, bf); err != nil {
			return rep, err
		}
		r.e.emit(ProgressEvent{
			Account: r.account(), Resource: resourceMail, Phase: kind,
			Done: done, Message: "enumerated",
		})
		if next == "" {
			break
		}
		cursor = next
		if err := ctx.Err(); err != nil {
			return rep, err
		}
	}

	// Both writes go in one transaction: a crash between them would leave a
	// finished backfill with no state, which restarts the whole enumeration
	// with a fresh state token and throws away the original replay window.
	now := time.Now()
	bf.FinishedAt = &now
	if err := r.e.st.Tx(ctx, func(tx *store.Tx) error {
		if err := tx.SetBackfill(ctx, bf); err != nil {
			return err
		}
		return tx.SetState(ctx, r.account(), resourceMail, bf.StateAtStart)
	}); err != nil {
		return rep, err
	}
	rep.StateAfter = bf.StateAtStart
	rep.Duration = time.Since(started)
	r.finish(ctx, kind, started, rep, nil)

	// Replay everything that changed while we were enumerating.
	d, err := r.delta(ctx, bf.StateAtStart)
	rep.add(d)
	if d != nil && d.StateAfter != "" {
		rep.StateAfter = d.StateAfter
	}
	rep.Duration = time.Since(started)
	return rep, err
}

func (r *mailRun) finish(ctx context.Context, kind string, started time.Time, rep *ResourceReport, err error) {
	r.e.logSync(ctx, r.account(), kind, started, rep, err)
}

// ---------------------------------------------------------------------------
// Delta (DESIGN.md §7.2)

type deltaKind byte

const (
	// opNone is the zero value: an id nothing has classified yet.
	opNone deltaKind = iota
	opAdded
	opUpdated
	opRemoved
)

type deltaOp struct {
	kind deltaKind
	env  provider.Envelope
}

// coalesce collapses the provider's change list per remote id: a removal beats
// everything, and an add beats an update (we re-fetch the message either way).
func coalesce(ch *provider.Changes) ([]string, map[string]*deltaOp) {
	ops := make(map[string]*deltaOp, len(ch.Added)+len(ch.Updated)+len(ch.Removed))
	var order []string
	touch := func(id string) *deltaOp {
		if op, ok := ops[id]; ok {
			return op
		}
		op := &deltaOp{}
		ops[id] = op
		order = append(order, id)
		return op
	}
	for _, env := range ch.Updated {
		op := touch(env.RemoteID)
		if op.kind == opNone || op.kind == opUpdated {
			op.kind = opUpdated
			op.env = env
		}
	}
	for _, env := range ch.Added {
		op := touch(env.RemoteID)
		op.kind = opAdded
		op.env = env
	}
	for _, id := range ch.Removed {
		// A removal beats everything else the provider said about the id.
		touch(id).kind = opRemoved
	}
	return order, ops
}

// delta applies everything that changed since state.
func (r *mailRun) delta(ctx context.Context, since string) (*ResourceReport, error) {
	started := time.Now()
	rep := &ResourceReport{Kind: KindDelta, StateBefore: since}

	ch, err := r.mp.Changes(ctx, since)
	if errors.Is(err, provider.ErrStateExpired) {
		r.e.log.Warn("sync state expired, reconciling", "account", r.account())
		return r.reconcile(ctx, since)
	}
	if err != nil {
		return rep, fmt.Errorf("sync: %s: changes: %w", r.account(), err)
	}

	if ch.MailboxesChanged || r.e.nthDelta(r.account())%mailboxRefreshEvery == 0 {
		if err := r.syncMailboxes(ctx); err != nil {
			return rep, err
		}
	}

	ef, canRefresh := r.mp.(envelopeFetcher)

	order, ops := coalesce(ch)
	var fetch, removed, refresh []string
	var updated []provider.Envelope
	for _, id := range order {
		op := ops[id]
		switch op.kind {
		case opAdded:
			if r.rawMax > 0 {
				st, err := r.e.st.MessageIndexState(ctx, r.account(), id)
				if err != nil {
					return rep, err
				}
				// An existing envelope-only row is an earlier pass's decision
				// that this message is over raw_max_size — the size the change
				// list carries is only a hint, and Gmail's "added" records
				// carry none at all.
				oversize := op.env.Size > r.rawMax || (st.Exists && !st.RawComplete)
				if oversize && !st.Exists {
					if err := r.writeBatch(ctx, []*pendingMsg{r.stub(op.env)}, nil); err != nil {
						return rep, err
					}
					rep.Added++
					continue
				}
				if oversize {
					// Gmail reports an ordinary label change on a large message
					// as an "added" record, and downloading the whole thing for
					// that would defeat raw_max_size. Refresh what changed
					// instead: from the envelope when it carries the current
					// state, otherwise through a cheap envelope fetch.
					switch {
					case len(op.env.Mailboxes) > 0:
						updated = append(updated, op.env)
					case canRefresh:
						refresh = append(refresh, id)
					default:
						r.e.log.Debug("oversize message re-announced; leaving the stub as it is",
							"account", r.account(), "message", id)
					}
					continue
				}
			}
			fetch = append(fetch, id)
		case opUpdated:
			exists, _, err := r.e.st.HasMessage(ctx, r.account(), id)
			if err != nil {
				return rep, err
			}
			if !exists {
				// An update for something we never indexed: fetch it whole.
				fetch = append(fetch, id)
				continue
			}
			updated = append(updated, op.env)
		case opRemoved:
			removed = append(removed, id)
		}
	}

	setState := func(tx *store.Tx) error {
		if ch.NewState == "" {
			return nil
		}
		return tx.SetState(ctx, r.account(), resourceMail, ch.NewState)
	}

	// The state may only be advanced by the *last* write of the pass. Ids the
	// provider announced as added but could not produce are a real deletion for
	// anything already indexed, and they are only known once the fetch has
	// finished — so as soon as there is anything to fetch, the state moves in a
	// later transaction, never with a fetch batch. A crash then replays the
	// pass instead of losing the deletion.
	var fetchTail func(*store.Tx) error
	if len(fetch) == 0 && len(updated) == 0 && len(removed) == 0 && len(refresh) == 0 {
		fetchTail = setState
	}
	stateAdvanced := fetchTail != nil

	added := 0
	gone, err := r.fetchIndex(ctx, fetch, KindDelta, &added, fetchTail)
	rep.Added = added
	if err != nil {
		r.finish(ctx, KindDelta, started, rep, err)
		return rep, err
	}
	removed = append(removed, gone...)

	if len(refresh) > 0 {
		n, err := r.refreshEnvelopes(ctx, ef, refresh)
		rep.Updated += n
		if err != nil {
			r.finish(ctx, KindDelta, started, rep, err)
			return rep, err
		}
	}

	if len(updated) > 0 || len(removed) > 0 {
		if len(updated) > 0 {
			groups := make([][]string, 0, len(updated))
			for _, env := range updated {
				groups = append(groups, env.Mailboxes)
			}
			r.noteUnknown(ctx, groups...)
		}
		batches := chunk(updated, indexBatch)
		for i, b := range batches {
			last := i == len(batches)-1 && len(removed) == 0
			b := b
			err := r.e.st.Tx(ctx, func(tx *store.Tx) error {
				for _, env := range b {
					err := tx.UpdateMessageState(ctx, r.account(), env.RemoteID, env.Flags, nonNil(env.Mailboxes))
					if errors.Is(err, model.ErrNotFound) {
						continue
					}
					if err != nil {
						return err
					}
					rep.Updated++
				}
				if last {
					return setState(tx)
				}
				return nil
			})
			if err != nil {
				return rep, err
			}
			if last {
				stateAdvanced = true
			}
		}
		if len(removed) > 0 {
			err := r.e.st.Tx(ctx, func(tx *store.Tx) error {
				n, err := tx.MarkDeleted(ctx, r.account(), removed)
				if err != nil {
					return err
				}
				rep.Removed += n
				return setState(tx)
			})
			if err != nil {
				return rep, err
			}
			stateAdvanced = true
		}
	}
	if !stateAdvanced {
		// Nothing carried the state along (a pass that only fetched, or only
		// refreshed envelopes): move it now that all of the work is committed.
		if err := r.e.st.Tx(ctx, setState); err != nil {
			return rep, err
		}
	}

	rep.StateAfter = ch.NewState
	if rep.StateAfter == "" {
		rep.StateAfter = since
	}
	rep.Duration = time.Since(started)
	r.e.emit(ProgressEvent{
		Account: r.account(), Resource: resourceMail, Phase: KindDelta,
		Done: rep.Added + rep.Updated + rep.Removed, Message: "applied",
	})
	if rep.Added+rep.Updated+rep.Removed > 0 {
		r.finish(ctx, KindDelta, started, rep, nil)
	}
	return rep, nil
}

// nthDelta counts deltas per account so mailboxes get re-synced periodically
// even when the provider never flags a change.
func (e *Engine) nthDelta(account string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.deltaCount == nil {
		e.deltaCount = map[string]int{}
	}
	e.deltaCount[account]++
	return e.deltaCount[account]
}

// ---------------------------------------------------------------------------
// Reconcile (DESIGN.md §7.3)

// reconcile diffs the provider's full id list against the index. It is slow
// and logged loudly because it should be rare.
func (r *mailRun) reconcile(ctx context.Context, before string) (*ResourceReport, error) {
	started := time.Now()
	rep := &ResourceReport{Kind: KindReconcile, StateBefore: before}
	r.e.log.Warn("reconciling account", "account", r.account())

	// The new state has to be captured before the enumeration, exactly like a
	// backfill, or changes made while we walk the list are lost.
	newState, err := r.mp.State(ctx)
	if err != nil {
		return rep, fmt.Errorf("sync: %s: state: %w", r.account(), err)
	}
	if err := r.syncMailboxes(ctx); err != nil {
		return rep, err
	}

	localAll, err := r.e.st.ListRemoteIDs(ctx, r.account(), true)
	if err != nil {
		return rep, err
	}
	localLive, err := r.e.st.ListRemoteIDs(ctx, r.account(), false)
	if err != nil {
		return rep, err
	}
	all := make(map[string]bool, len(localAll))
	for _, id := range localAll {
		all[id] = true
	}
	live := make(map[string]bool, len(localLive))
	for _, id := range localLive {
		live[id] = true
	}

	// A provider that can hand back current flags/mailboxes without the body
	// (Gmail) lets a resurrected message be refiled cheaply; one that cannot
	// (JMAP) needs the envelope's own mailbox list, or a full re-index.
	ef, canRefresh := r.mp.(envelopeFetcher)

	remote := make(map[string]bool, len(localAll))
	var fetch []string
	var stubs []*pendingMsg
	var existing []string
	var resurrect []provider.Envelope
	cursor := ""

	for {
		page, next, err := r.mp.Enumerate(ctx, cursor, enumeratePage)
		if err != nil {
			r.finish(ctx, KindReconcile, started, rep, err)
			return rep, fmt.Errorf("sync: %s: enumerate: %w", r.account(), err)
		}
		for _, env := range page {
			remote[env.RemoteID] = true
			switch {
			case !all[env.RemoteID]:
				if r.rawMax > 0 && env.Size > r.rawMax {
					stubs = append(stubs, r.stub(env))
				} else {
					fetch = append(fetch, env.RemoteID)
				}
			default:
				if !live[env.RemoteID] {
					// It came back. Whatever brings its mailbox membership back
					// has to run too, or it is un-deleted and in no mailbox.
					oversize := r.rawMax > 0 && env.Size > r.rawMax
					if len(env.Mailboxes) == 0 && !canRefresh && !oversize {
						fetch = append(fetch, env.RemoteID) // the upsert un-deletes
						continue
					}
					resurrect = append(resurrect, env)
				}
				existing = append(existing, env.RemoteID)
			}
		}

		r.e.emit(ProgressEvent{
			Account: r.account(), Resource: resourceMail, Phase: KindReconcile,
			Done: len(remote), Message: "enumerated",
		})
		if next == "" {
			break
		}
		cursor = next
		if err := ctx.Err(); err != nil {
			return rep, err
		}
	}

	if err := r.resurrect(ctx, resurrect); err != nil {
		return rep, err
	}

	for _, b := range chunk(stubs, indexBatch) {
		if err := r.writeBatch(ctx, b, nil); err != nil {
			return rep, err
		}
		rep.Added += len(b)
	}

	added := 0
	gone, err := r.fetchIndex(ctx, fetch, KindReconcile, &added, nil)
	rep.Added += added
	if err != nil {
		r.finish(ctx, KindReconcile, started, rep, err)
		return rep, err
	}

	var missing []string
	for _, id := range localLive {
		if !remote[id] {
			missing = append(missing, id)
		}
	}
	missing = append(missing, gone...)
	if len(missing) > 0 {
		n, err := r.e.st.MarkDeleted(ctx, r.account(), missing)
		if err != nil {
			return rep, err
		}
		rep.Removed += n
	}

	// Refresh flags/mailboxes for everything we already had, when the provider
	// offers a cheap way to ask.
	if canRefresh {
		n, err := r.refreshEnvelopes(ctx, ef, existing)
		rep.Updated += n
		if err != nil {
			r.finish(ctx, KindReconcile, started, rep, err)
			return rep, err
		}
	} else if len(existing) > 0 {
		r.e.log.Warn("provider cannot refresh envelopes cheaply; flags/mailboxes of existing messages are left as they are",
			"account", r.account(), "messages", len(existing))
	}

	if err := r.e.st.SetState(ctx, r.account(), resourceMail, newState); err != nil {
		return rep, err
	}
	// A reconcile supersedes any half-finished backfill.
	if bf, err := r.e.st.GetBackfill(ctx, r.account(), resourceMail); err == nil && !bf.Finished() {
		now := time.Now()
		bf.FinishedAt = &now
		bf.Cursor = ""
		if err := r.e.st.SetBackfill(ctx, bf); err != nil {
			return rep, err
		}
	}

	rep.StateAfter = newState
	rep.Duration = time.Since(started)
	r.finish(ctx, KindReconcile, started, rep, nil)
	return rep, nil
}

// refreshEnvelopes re-applies current flags/mailboxes for ids in chunks.
func (r *mailRun) refreshEnvelopes(ctx context.Context, ef envelopeFetcher, ids []string) (int, error) {
	updated := 0
	for _, part := range chunk(ids, envelopeChunk) {
		var envs []provider.Envelope
		if err := ef.FetchEnvelopes(ctx, part, func(env provider.Envelope) error {
			envs = append(envs, env)
			return nil
		}); err != nil {
			return updated, fmt.Errorf("sync: %s: envelopes: %w", r.account(), err)
		}
		groups := make([][]string, 0, len(envs))
		for _, env := range envs {
			groups = append(groups, env.Mailboxes)
		}
		r.noteUnknown(ctx, groups...)

		err := r.e.st.Tx(ctx, func(tx *store.Tx) error {
			for _, env := range envs {
				err := tx.UpdateMessageState(ctx, r.account(), env.RemoteID, env.Flags, nonNil(env.Mailboxes))
				if errors.Is(err, model.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				updated++
			}
			return nil
		})
		if err != nil {
			return updated, err
		}
		r.e.emit(ProgressEvent{
			Account: r.account(), Resource: resourceMail, Phase: KindReconcile,
			Done: updated, Total: len(ids), Message: "refreshed",
		})
	}
	return updated, nil
}
