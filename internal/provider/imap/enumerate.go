package imap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// enumCursor is where an enumeration got to. The folder list is frozen when the
// cursor is made: a folder created mid-backfill is missed here, but the engine
// replays a delta from the state captured before the enumeration, and that
// state has no entry for it — so every message in it arrives as Added. Correct
// by construction, and it keeps the walk from chasing a moving target.
type enumCursor struct {
	V     int      `json:"v"`
	Order []string `json:"o"`
	I     int      `json:"i"`
	UIDs  string   `json:"u"`  // still to emit from Order[I]
	Val   uint32   `json:"vy"` // UIDVALIDITY of Order[I] when it was listed
	N     int      `json:"n"`
}

func (c enumCursor) String() string {
	c.V = stateVersion
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseEnumCursor(s string) (enumCursor, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return enumCursor{V: stateVersion}, nil
	}
	var c enumCursor
	if err := json.Unmarshal([]byte(s), &c); err != nil || c.V != stateVersion {
		return enumCursor{}, fmt.Errorf("imap: bad enumerate cursor %q", s)
	}
	return c, nil
}

// State captures the account's current position, UID sets and all.
//
// The extra UID SEARCH per folder is what makes backfill_progress.state_at_start
// an exact replay window: mail that arrives during a six-hour backfill moves
// UIDNEXT, mail expunged during it shrinks the set, and the delta the engine
// replays afterwards reports both correctly.
func (m *Mail) State(ctx context.Context) (string, error) {
	folders, err := m.syncedFolders(ctx)
	if err != nil {
		return "", err
	}
	st := newMailState()
	for _, f := range folders {
		fs, err := m.snapshot(ctx, f.Name)
		if err != nil {
			return "", err
		}
		st.Folders[f.Name] = fs
	}
	return st.String(), nil
}

// snapshot measures one folder: its identity, its size and exactly which uids
// it holds.
func (m *Mail) snapshot(ctx context.Context, mailbox string) (folderState, error) {
	var fs folderState
	err := m.pool.with(ctx, mailbox, true, func(c *conn) error {
		if c.sel == nil {
			return fmt.Errorf("imap: %s: no select data", mailbox)
		}
		fs.UIDVal = c.sel.UIDValidity
		fs.UIDNext = uint32(c.sel.UIDNext)
		fs.Count = c.sel.NumMessages
		fs.ModSeq = c.sel.HighestModSeq

		// FETCH rather than SEARCH: one round trip gives both the uid set and
		// the flags, and the flags are what let the next delta tell a change
		// from a re-read.
		uids, sets, err := fetchAllFlags(c)
		if err != nil {
			return err
		}
		fs.UIDs = encodeUIDs(uids)
		sets.encodeInto(&fs)
		fs.Unseen = uint32(len(uids) - len(sets.seen))
		if fs.UIDNext == 0 {
			// Old servers may omit UIDNEXT from SELECT; derive a floor from
			// what is actually there so the next pass does not rescan.
			for _, u := range uids {
				if uint32(u)+1 > fs.UIDNext {
					fs.UIDNext = uint32(u) + 1
				}
			}
		}
		return nil
	})
	return fs, err
}

// fetchAllFlags lists every uid in the selected mailbox along with its flags.
func fetchAllFlags(c *conn) ([]imapv2.UID, flagSets, error) {
	sets := newFlagSets()
	if c.sel != nil && c.sel.NumMessages == 0 {
		return nil, sets, nil
	}
	var set imapv2.UIDSet
	set.AddRange(1, 0) // 1:*
	msgs, err := c.c.Fetch(set, &imapv2.FetchOptions{UID: true, Flags: true}).Collect()
	if err != nil {
		return nil, sets, c.fail(wrapErr("fetch flags in "+c.selected, err))
	}
	uids := make([]imapv2.UID, 0, len(msgs))
	for _, msg := range msgs {
		uids = append(uids, msg.UID)
		sets.set(msg.UID, flagsFrom(msg.Flags))
	}
	return uids, sets, nil
}

// searchAll lists every uid in the selected mailbox.
func searchAll(c *conn) ([]imapv2.UID, error) {
	data, err := c.c.UIDSearch(&imapv2.SearchCriteria{}, &imapv2.SearchOptions{
		ReturnAll: hasCap(c.caps, imapv2.CapESearch),
	}).Wait()
	if err != nil {
		return nil, c.fail(wrapErr("search "+c.selected, err))
	}
	return data.AllUIDs(), nil
}

// Enumerate walks the account folder by folder, newest mail first.
func (m *Mail) Enumerate(ctx context.Context, cursor string, limit int) ([]provider.Envelope, string, error) {
	cur, err := parseEnumCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		limit = 200
	}

	if cur.Order == nil {
		folders, err := m.syncedFolders(ctx)
		if err != nil {
			return nil, "", err
		}
		cur.Order = make([]string, 0, len(folders))
		for _, f := range folders {
			cur.Order = append(cur.Order, f.Name)
		}
		cur.I, cur.UIDs, cur.Val = 0, "", 0
	}

	for cur.I < len(cur.Order) {
		mailbox := cur.Order[cur.I]

		if cur.UIDs == "" && cur.Val == 0 {
			fs, err := m.snapshot(ctx, mailbox)
			if err != nil {
				// A folder that vanished or refuses SELECT mid-walk is skipped,
				// not fatal: the delta afterwards reconciles whatever it holds.
				m.log.Warn("skipping folder during enumerate", "mailbox", mailbox, "err", err)
				cur.I++
				cur.UIDs, cur.Val = "", 0
				continue
			}
			cur.UIDs, cur.Val = fs.UIDs, fs.UIDVal
			if cur.UIDs == "" {
				cur.I++
				cur.Val = 0
				continue
			}
		}

		uids, err := decodeUIDs(cur.UIDs)
		if err != nil {
			return nil, "", err
		}
		if len(uids) == 0 {
			cur.I++
			cur.UIDs, cur.Val = "", 0
			continue
		}

		// Newest first: take from the tail, which is the highest uids.
		n := min(limit, len(uids))
		page := uids[len(uids)-n:]
		rest := uids[:len(uids)-n]

		envs, err := m.envelopesFor(ctx, mailbox, cur.Val, page)
		if err != nil {
			return nil, "", err
		}

		cur.UIDs = encodeUIDs(rest)
		cur.N += len(envs)
		if len(rest) == 0 {
			cur.I++
			cur.UIDs, cur.Val = "", 0
		}
		next := cur.String()
		if cur.I >= len(cur.Order) {
			next = ""
		}
		return envs, next, nil
	}
	return nil, "", nil
}

// envelopesFor fetches the cheap per-message facts for a page of uids.
func (m *Mail) envelopesFor(ctx context.Context, mailbox string, validity uint32, uids []imapv2.UID) ([]provider.Envelope, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	var set imapv2.UIDSet
	for _, u := range uids {
		set.AddNum(u)
	}

	var out []provider.Envelope
	err := m.pool.with(ctx, mailbox, true, func(c *conn) error {
		if validity != 0 && c.sel != nil && c.sel.UIDValidity != validity {
			return errValidityChanged
		}
		msgs, err := c.c.Fetch(set, &imapv2.FetchOptions{
			UID: true, Flags: true, InternalDate: true, RFC822Size: true,
		}).Collect()
		if err != nil {
			return c.fail(wrapErr("fetch envelopes in "+mailbox, err))
		}
		for _, msg := range msgs {
			out = append(out, m.envelope(mailbox, c.sel.UIDValidity, msg))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// envelope maps one fetched message onto the engine's shape.
func (m *Mail) envelope(mailbox string, validity uint32, msg *imapclient.FetchMessageBuffer) provider.Envelope {
	r := ref{Mailbox: mailbox, UIDValidity: validity, UID: msg.UID}
	return provider.Envelope{
		RemoteID:  r.String(),
		Received:  msg.InternalDate,
		Size:      msg.RFC822Size,
		Flags:     flagsFrom(msg.Flags),
		Mailboxes: []string{mailbox},
	}
}

// flagsFrom maps IMAP flags onto the normalised set. \Seen is inverted: the
// model records unread, IMAP records read.
func flagsFrom(flags []imapv2.Flag) model.Flags {
	out := model.Flags{Unread: true}
	for _, f := range flags {
		switch f {
		case imapv2.FlagSeen:
			out.Unread = false
		case imapv2.FlagFlagged:
			out.Flagged = true
		case imapv2.FlagDraft:
			out.Draft = true
		case imapv2.FlagAnswered:
			out.Answered = true
		}
	}
	return out
}
