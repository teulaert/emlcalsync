package imap

import (
	"context"
	"fmt"
	"sort"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/teulaert/emlcalsync/internal/provider"
)

// changesMaxAdded is where a delta gives up and asks for a reconcile. One
// Changes result is held whole in memory and applied in one pass; past this
// the engine's paged, resumable backfill is the better tool.
const changesMaxAdded = 20000

// flagSweepEvery is how many passes go by between full flag rescans on a server
// with no CONDSTORE.
const flagSweepEvery = 12

// flagWindow is how many of the newest uids get their flags rechecked every
// pass on such a server. Flag changes happen to recent mail; this keeps the
// cost constant instead of proportional to the folder.
const flagWindow = 2000

// Changes reports what moved since the given state.
//
// Everything a server can do to a folder — new mail, expunges, a UIDVALIDITY
// reset, being renamed, being deleted — is expressible as exact Added/Removed
// here, because the state carries the uid set we last reported. So
// ErrStateExpired is genuinely rare: a state this build cannot read, or a
// change too large to hand over in one piece.
func (m *Mail) Changes(ctx context.Context, since string) (*provider.Changes, error) {
	prev, ok := parseMailState(since)
	if !ok {
		return nil, provider.ErrStateExpired
	}

	folders, err := m.freshSyncedFolders(ctx)
	if err != nil {
		return nil, err
	}
	live := make(map[string]folder, len(folders))
	names := make([]string, 0, len(folders))
	for _, f := range folders {
		live[f.Name] = f
		names = append(names, f.Name)
	}
	sort.Strings(names)

	ch := &provider.Changes{}
	next := newMailState()
	next.Pass = prev.Pass + 1

	// Folders that were in the state and are no longer listed.
	var gone []string
	for name := range prev.Folders {
		if _, still := live[name]; !still {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)

	// A folder that is new while another vanished, with the same identity and
	// size, is a rename. Reporting it as such keeps the messages and their
	// bodies; the alternative is deleting and re-downloading a whole folder
	// because somebody renamed it.
	var fresh []string
	for _, name := range names {
		if _, known := prev.Folders[name]; !known {
			fresh = append(fresh, name)
		}
	}
	renamedFrom := map[string]string{}
	if len(gone) == 1 && len(fresh) == 1 {
		oldState := prev.Folders[gone[0]]
		newState, err := m.snapshot(ctx, fresh[0])
		if err == nil && newState.UIDVal == oldState.UIDVal && newState.Count == oldState.Count {
			oldUIDs, _ := decodeUIDs(oldState.UIDs)
			newIdx, _ := uidIndex(newState.UIDs)
			allThere := true
			for _, u := range oldUIDs {
				if !newIdx[u] {
					allThere = false
					break
				}
			}
			if allThere {
				for _, u := range oldUIDs {
					ch.Renamed = append(ch.Renamed, provider.Rename{
						Old: ref{Mailbox: gone[0], UIDValidity: oldState.UIDVal, UID: u}.String(),
						New: ref{Mailbox: fresh[0], UIDValidity: newState.UIDVal, UID: u}.String(),
					})
				}
				renamedFrom[fresh[0]] = gone[0]
				next.Folders[fresh[0]] = newState
				gone = nil
				ch.MailboxesChanged = true
			}
		}
	}

	// Whatever is still missing really is gone.
	for _, name := range gone {
		fs := prev.Folders[name]
		uids, err := decodeUIDs(fs.UIDs)
		if err != nil {
			return nil, provider.ErrStateExpired
		}
		for _, u := range uids {
			ch.Removed = append(ch.Removed, ref{Mailbox: name, UIDValidity: fs.UIDVal, UID: u}.String())
		}
		ch.MailboxesChanged = true
	}

	for _, name := range names {
		if _, done := next.Folders[name]; done {
			continue // handled by the rename above
		}
		fs, ok := prev.Folders[name]
		if !ok {
			// A folder we have never seen: everything in it is new.
			ch.MailboxesChanged = true
			snap, err := m.snapshot(ctx, name)
			if err != nil {
				m.log.Warn("skipping unreadable folder", "mailbox", name, "err", err)
				continue
			}
			uids, err := decodeUIDs(snap.UIDs)
			if err != nil {
				return nil, err
			}
			envs, err := m.envelopesFor(ctx, name, snap.UIDVal, uids)
			if err != nil {
				return nil, err
			}
			ch.Added = append(ch.Added, envs...)
			next.Folders[name] = snap
			continue
		}

		fstate, err := m.folderChanges(ctx, name, fs, next.Pass, ch)
		if err != nil {
			// A permission blip or a folder that briefly refuses SELECT must
			// not be read as "every message in it was deleted". Keep what we
			// knew and try again next pass.
			m.log.Warn("folder unreadable this pass, leaving its state alone", "mailbox", name, "err", err)
			next.Folders[name] = fs
			continue
		}
		next.Folders[name] = fstate

		if len(ch.Added) > changesMaxAdded {
			m.log.Warn("too much changed for one delta, asking for a reconcile",
				"account", m.opts.Email, "added", len(ch.Added))
			return nil, provider.ErrStateExpired
		}
	}

	ch.NewState = next.String()
	return ch, nil
}

// folderChanges diffs one folder against what we last knew of it, appending to
// ch and returning the folder's new state.
func (m *Mail) folderChanges(ctx context.Context, mailbox string, prev folderState, pass int, ch *provider.Changes) (folderState, error) {
	var out folderState

	status, err := m.status(ctx, mailbox)
	if err != nil {
		return out, err
	}

	// The folder was recreated: its uids now mean something else, so everything
	// we held under the old validity is gone and everything present is new.
	if status.UIDVal != prev.UIDVal {
		oldUIDs, err := decodeUIDs(prev.UIDs)
		if err != nil {
			return out, err
		}
		for _, u := range oldUIDs {
			ch.Removed = append(ch.Removed, ref{Mailbox: mailbox, UIDValidity: prev.UIDVal, UID: u}.String())
		}
		snap, err := m.snapshot(ctx, mailbox)
		if err != nil {
			return out, err
		}
		uids, err := decodeUIDs(snap.UIDs)
		if err != nil {
			return out, err
		}
		envs, err := m.envelopesFor(ctx, mailbox, snap.UIDVal, uids)
		if err != nil {
			return out, err
		}
		ch.Added = append(ch.Added, envs...)
		m.log.Warn("uidvalidity changed, refiling the folder",
			"mailbox", mailbox, "was", prev.UIDVal, "now", snap.UIDVal, "messages", len(uids))
		return snap, nil
	}

	condstore := prev.ModSeq != 0 && status.ModSeq != 0
	quiet := status.UIDNext == prev.UIDNext && status.Count == prev.Count &&
		status.Unseen == prev.Unseen &&
		(!condstore || status.ModSeq == prev.ModSeq)

	// Nothing arrived, nothing left, and — where the server can say so —
	// nothing changed. This is the common case and it costs no SELECT.
	needSweep := !condstore && pass-prev.Scan >= flagSweepEvery
	if quiet && !needSweep {
		out = prev
		out.Count, out.UIDNext, out.ModSeq = status.Count, status.UIDNext, status.ModSeq
		out.Unseen = status.Unseen
		return out, nil
	}

	prevIdx, err := uidIndex(prev.UIDs)
	if err != nil {
		return out, err
	}
	sets, err := prev.flagSets()
	if err != nil {
		return out, err
	}

	out = prev
	err = m.pool.with(ctx, mailbox, true, func(c *conn) error {
		if c.sel == nil {
			return fmt.Errorf("imap: %s: no select data", mailbox)
		}
		validity := c.sel.UIDValidity

		// Arrivals: everything at or above the uid we had not seen yet.
		var addedUIDs []imapv2.UID
		if status.UIDNext > prev.UIDNext || status.Count > prev.Count {
			var set imapv2.UIDSet
			set.AddRange(imapv2.UID(prev.UIDNext), 0) // n:* — 0 means *
			msgs, err := c.c.Fetch(set, &imapv2.FetchOptions{
				UID: true, Flags: true, InternalDate: true, RFC822Size: true,
			}).Collect()
			if err != nil {
				return c.fail(wrapErr("fetch new in "+mailbox, err))
			}
			for _, msg := range msgs {
				if prevIdx[msg.UID] {
					continue
				}
				addedUIDs = append(addedUIDs, msg.UID)
				sets.set(msg.UID, flagsFrom(msg.Flags))
				ch.Added = append(ch.Added, m.envelope(mailbox, validity, msg))
			}
		}

		// Flag changes.
		changed, err := m.changedFlags(c, mailbox, prev, status, condstore, needSweep)
		if err != nil {
			return err
		}
		for _, msg := range changed {
			if !prevIdx[msg.UID] {
				continue // an arrival, already reported above
			}
			now := flagsFrom(msg.Flags)
			// Only what actually moved. Without CONDSTORE we re-read flags we
			// may well have read before, and reporting those as updates would
			// churn the index on an account where nothing happened.
			if now == sets.flagsOf(msg.UID) {
				continue
			}
			sets.set(msg.UID, now)
			ch.Updated = append(ch.Updated, m.envelope(mailbox, validity, msg))
		}

		// Expunges. Counting is enough to rule them out in the common case,
		// which saves a SEARCH over the whole folder on every poll.
		expectedCount := uint32(len(prevIdx) + len(addedUIDs))
		if status.Count == expectedCount && !needSweep {
			live := make([]imapv2.UID, 0, expectedCount)
			for u := range prevIdx {
				live = append(live, u)
			}
			live = append(live, addedUIDs...)
			out.UIDs = encodeUIDs(live)
		} else {
			liveUIDs, err := searchAll(c)
			if err != nil {
				return err
			}
			liveIdx := make(map[imapv2.UID]bool, len(liveUIDs))
			for _, u := range liveUIDs {
				liveIdx[u] = true
			}
			for u := range prevIdx {
				if !liveIdx[u] {
					ch.Removed = append(ch.Removed,
						ref{Mailbox: mailbox, UIDValidity: prev.UIDVal, UID: u}.String())
					sets.forget(u)
				}
			}
			out.UIDs = encodeUIDs(liveUIDs)
		}
		return nil
	})
	if err != nil {
		return folderState{}, err
	}

	out.UIDVal = status.UIDVal
	out.UIDNext = status.UIDNext
	out.ModSeq = status.ModSeq
	out.Count = status.Count
	out.Unseen = status.Unseen
	sets.encodeInto(&out)
	if needSweep {
		out.Scan = pass
	}
	return out, nil
}

// changedFlags fetches the messages whose flags may have moved.
//
// With CONDSTORE this is exact and cheap. Without it nothing on the wire says
// which flags changed, so we recheck the newest flagWindow uids every pass —
// bounded work, and where flag changes actually happen — and sweep the whole
// folder occasionally.
func (m *Mail) changedFlags(c *conn, mailbox string, prev folderState, status folderState, condstore, sweep bool) ([]*imapclient.FetchMessageBuffer, error) {
	var set imapv2.UIDSet
	opts := &imapv2.FetchOptions{UID: true, Flags: true, InternalDate: true, RFC822Size: true}

	switch {
	case condstore:
		if status.ModSeq == prev.ModSeq {
			return nil, nil
		}
		set.AddRange(1, 0)
		opts.ChangedSince = prev.ModSeq
	case sweep:
		set.AddRange(1, 0)
	default:
		lo := imapv2.UID(1)
		if status.UIDNext > flagWindow {
			lo = imapv2.UID(status.UIDNext - flagWindow)
		}
		set.AddRange(lo, 0)
	}

	msgs, err := c.c.Fetch(set, opts).Collect()
	if err != nil {
		return nil, c.fail(wrapErr("fetch flags in "+mailbox, err))
	}
	return msgs, nil
}

// status measures a folder without selecting it.
func (m *Mail) status(ctx context.Context, mailbox string) (folderState, error) {
	var out folderState
	err := m.pool.withAny(ctx, func(c *conn) error {
		st, err := c.c.Status(mailbox, &imapv2.StatusOptions{
			NumMessages: true, UIDNext: true, UIDValidity: true, NumUnseen: true,
			HighestModSeq: hasCap(c.caps, imapv2.CapCondStore),
		}).Wait()
		if err != nil {
			return c.fail(wrapErr("status "+mailbox, err))
		}
		out.UIDVal = st.UIDValidity
		out.UIDNext = uint32(st.UIDNext)
		out.ModSeq = st.HighestModSeq
		if st.NumMessages != nil {
			out.Count = *st.NumMessages
		}
		if st.NumUnseen != nil {
			out.Unseen = *st.NumUnseen
		}
		return nil
	})
	return out, err
}
