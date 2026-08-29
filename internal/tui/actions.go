package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/store"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// target is one message an action applies to, carrying the state undo needs.
type target struct {
	account string
	remote  string
	// mailboxes is the message's membership set before the action. Trash wipes
	// it (mailboxPatch sets clearOthers), so an exact undo is only possible if
	// it was captured first.
	mailboxes []string
	flags     model.Flags
}

func targetOf(m *model.Message) target {
	return target{
		account:   m.AccountID,
		remote:    m.RemoteID,
		mailboxes: append([]string(nil), m.MailboxRemotes...),
		flags:     m.Flags,
	}
}

// undoRecord is the inverse of an action, ready to hand back to Engine.Apply.
//
// The engine has no undo primitive: Engine.rollback runs only inside Apply,
// when a provider *rejects* a write. So undo is a second ordinary Apply, and
// knowing the inverse is this package's job.
type undoRecord struct {
	label string
	when  time.Time
	ops   []accountOp
}

type accountOp struct {
	account string
	op      sync.Op
}

// undoWindow is how long the offer stays on the status line.
const undoWindow = 15 * time.Second

func (u *undoRecord) live(now time.Time) bool {
	return u != nil && now.Sub(u.when) < undoWindow
}

// ---------------------------------------------------------------------------
// Building the ops

// group collects targets by account, because Engine.Apply takes one account at
// a time. This mirrors cli.App.GroupMessageIDs, which cannot be reused: it is
// a method on *cli.App and internal/cli imports this package.
func group(ts []target) []struct {
	account string
	remotes []string
} {
	var out []struct {
		account string
		remotes []string
	}
	idx := map[string]int{}
	for _, t := range ts {
		i, ok := idx[t.account]
		if !ok {
			i = len(out)
			idx[t.account] = i
			out = append(out, struct {
				account string
				remotes []string
			}{account: t.account})
		}
		out[i].remotes = append(out[i].remotes, t.remote)
	}
	return out
}

// roleRemote finds an account's mailbox for a role. Undo needs it by name
// because OpArchive resolves the roles itself and does not report what it used.
func roleRemote(ctx context.Context, st *store.Store, account string, role model.MailboxRole) string {
	mbs, err := st.ListMailboxes(ctx, account)
	if err != nil {
		return ""
	}
	for _, m := range mbs {
		if m.Role == role {
			return m.RemoteID
		}
	}
	return ""
}

// archiveOps builds the archive, plus the inverse that puts it back.
func archiveOps(ctx context.Context, st *store.Store, ts []target) ([]accountOp, *undoRecord) {
	var fwd, back []accountOp
	for _, g := range group(ts) {
		fwd = append(fwd, accountOp{g.account, sync.Op{Kind: sync.OpArchive, IDs: g.remotes}})
		inbox := roleRemote(ctx, st, g.account, model.RoleInbox)
		archive := roleRemote(ctx, st, g.account, model.RoleArchive)
		op := sync.Op{Kind: sync.OpMailboxes, IDs: g.remotes}
		if inbox != "" {
			op.AddMailboxes = []string{inbox}
		}
		if archive != "" {
			op.RemoveMailboxes = []string{archive}
		}
		back = append(back, accountOp{g.account, op})
	}
	return fwd, &undoRecord{label: "archive", ops: back}
}

// trashOps builds the trash, plus the inverse.
//
// OpTrash sets clearOthers, so every other membership is dropped locally; the
// only honest undo is to replay the set each message had beforehand. Trash
// itself is recoverable on both providers — Gmail adds the TRASH label, JMAP
// files into the trash mailbox — so the message really does come back.
func trashOps(ctx context.Context, st *store.Store, ts []target) ([]accountOp, *undoRecord) {
	var fwd, back []accountOp
	for _, g := range group(ts) {
		fwd = append(fwd, accountOp{g.account, sync.Op{Kind: sync.OpTrash, IDs: g.remotes}})
	}
	trashOf := map[string]string{}
	// One op per message, because each may have had a different membership set.
	for _, t := range ts {
		trash, ok := trashOf[t.account]
		if !ok {
			trash = roleRemote(ctx, st, t.account, model.RoleTrash)
			trashOf[t.account] = trash
		}
		op := sync.Op{Kind: sync.OpMailboxes, IDs: []string{t.remote}}
		op.AddMailboxes = append([]string(nil), t.mailboxes...)
		if trash != "" {
			op.RemoveMailboxes = []string{trash}
		}
		back = append(back, accountOp{t.account, op})
	}
	return fwd, &undoRecord{label: "trash", ops: back}
}

// flagOps toggles a flag and builds the inverse.
func flagOps(ts []target, flag string, set bool) ([]accountOp, *undoRecord) {
	mk := func(on bool) []accountOp {
		var out []accountOp
		for _, g := range group(ts) {
			var op sync.Op
			op.Kind = sync.OpFlags
			op.IDs = g.remotes
			switch flag {
			case "unread":
				if on {
					op.Flags.Set.Unread = true
				} else {
					op.Flags.Clear.Unread = true
				}
			case "flagged":
				if on {
					op.Flags.Set.Flagged = true
				} else {
					op.Flags.Clear.Flagged = true
				}
			}
			out = append(out, accountOp{g.account, op})
		}
		return out
	}
	label := flag
	if !set {
		label = "un" + flag
	}
	return mk(set), &undoRecord{label: label, ops: mk(!set)}
}

// respondOp is the calendar RSVP.
func respondOp(accountID, calRemote, remote string, r model.Participation) []accountOp {
	return []accountOp{{accountID, sync.Op{
		Kind:           sync.OpEventRespond,
		CalendarRemote: calRemote,
		IDs:            []string{remote},
		Response:       r,
	}}}
}

// ---------------------------------------------------------------------------
// Running them

// apply runs every op and folds the results into one message.
//
// ApplyResult.Renames matters even though Gmail and JMAP always return it
// empty: on IMAP a move mints a new UID, so the id the caller passed in stops
// naming anything. Everything the UI is still holding — the selected row, the
// undo record, an open reader — has to be rewritten through it.
func (d Deps) apply(label string, ops []accountOp, undo *undoRecord) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res := applied{action: label, renames: map[string]string{}, undo: undo}
		for _, ao := range ops {
			r, err := d.Engine.Apply(ctx, ao.account, ao.op)
			if err != nil {
				res.err = fmt.Errorf("%s: %w", ao.account, err)
				res.undo = nil // Apply already rolled its own patch back.
				return res
			}
			res.account = ao.account
			if r.Queued {
				res.queued = true
			}
			for _, rn := range r.Renames {
				res.renames[model.MessagePublicID(ao.account, rn.Old)] =
					model.MessagePublicID(ao.account, rn.New)
			}
		}
		if res.undo != nil {
			res.undo.when = time.Now()
			res.undo.rewrite(res.renames)
		}
		return res
	}
}

// submit sends a composed message, or stores it as a draft.
//
// It follows `mail send`: the reply goes out first, and only once it really
// has -- not when it was merely queued -- is the message it answers marked
// answered and the stored draft it was written in replaces trashed. Neither
// of those may fail the send, so both are logged and dropped.
//
// replaces is the draft the new message supersedes, because no provider can
// update a draft in place: saving or sending one creates the new message and
// leaves the old one behind unless it is cleared up here.
func (d Deps) submit(what, account string, op sync.Op, orig *model.Message, replaces string) tea.Cmd {
	return func() tea.Msg {
		if d.Engine == nil {
			return submitted{what: what, err: errors.New("no sync engine")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := d.Engine.Apply(ctx, account, op)
		if err != nil {
			// A submission that never left the machine comes back queued, not
			// as an error. Being offline *here* means the connection dropped
			// mid-request, so the engine will not replay it: the provider may
			// already have the message, and saying "queued" would be a lie.
			if provider.IsOffline(err) {
				err = fmt.Errorf("%w — the connection dropped mid-request, so it was not queued; "+
					"check your sent mail before sending again", err)
			}
			return submitted{what: what, err: err}
		}
		if !res.Queued && orig != nil {
			var flag sync.Op
			flag.Kind = sync.OpFlags
			flag.IDs = []string{orig.RemoteID}
			flag.Flags.Set.Answered = true
			if _, err := d.Engine.Apply(ctx, orig.AccountID, flag); err != nil {
				d.log().Warn("mark answered", "id", orig.PublicID(), "err", err)
			}
		}
		if !res.Queued && replaces != "" && replaces != res.RemoteID {
			if _, err := d.Engine.Apply(ctx, account,
				sync.Op{Kind: sync.OpTrash, IDs: []string{replaces}}); err != nil {
				d.log().Warn("trash the draft it replaces",
					"id", model.MessagePublicID(account, replaces), "err", err)
			}
		}
		return submitted{what: what, queued: res.Queued}
	}
}

// rewrite maps the ids an undo record holds through a set of renames.
func (u *undoRecord) rewrite(renames map[string]string) {
	if u == nil || len(renames) == 0 {
		return
	}
	for i := range u.ops {
		acct := u.ops[i].account
		ids := u.ops[i].op.IDs
		for j, id := range ids {
			if to, ok := renames[model.MessagePublicID(acct, id)]; ok {
				if p, err := model.ParseID(to); err == nil {
					ids[j] = p.Remote
				}
			}
		}
	}
}
