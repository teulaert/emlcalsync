package imap

import (
	"context"
	"fmt"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// SetFlags applies flag changes, one folder at a time.
func (m *Mail) SetFlags(ctx context.Context, ids []string, set, clear model.Flags) error {
	add, remove := imapFlags(set), imapFlags(clear)
	// \Seen is inverted against the model: marking a message unread means
	// taking \Seen off, not putting it on.
	if set.Unread {
		remove = append(remove, imapv2.FlagSeen)
	}
	if clear.Unread {
		add = append(add, imapv2.FlagSeen)
	}
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}

	byMailbox, _ := groupRefs(ids)
	for mailbox, refs := range byMailbox {
		err := m.pool.with(ctx, mailbox, false, func(c *conn) error {
			for _, op := range []struct {
				kind  imapv2.StoreFlagsOp
				flags []imapv2.Flag
			}{{imapv2.StoreFlagsAdd, add}, {imapv2.StoreFlagsDel, remove}} {
				if len(op.flags) == 0 {
					continue
				}
				// No UNCHANGEDSINCE: emlcal is deliberately last-writer-wins
				// with an optimistic local patch the next delta reconciles. A
				// MODIFIED response would be read as a rejection and roll the
				// user's change back instead of writing it.
				cmd := c.c.Store(uidSet(refs), &imapv2.StoreFlags{
					Op: op.kind, Silent: true, Flags: op.flags,
				}, nil)
				if err := cmd.Close(); err != nil {
					return c.fail(wrapErr("store flags in "+mailbox, err))
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// imapFlags maps the model's flags onto IMAP's, minus Unread which inverts.
func imapFlags(f model.Flags) []imapv2.Flag {
	var out []imapv2.Flag
	if f.Flagged {
		out = append(out, imapv2.FlagFlagged)
	}
	if f.Draft {
		out = append(out, imapv2.FlagDraft)
	}
	if f.Answered {
		out = append(out, imapv2.FlagAnswered)
	}
	return out
}

// SetMailboxes moves messages between folders. Prefer SetMailboxesRemap: on
// IMAP a move mints new uids, and this signature cannot report them.
func (m *Mail) SetMailboxes(ctx context.Context, ids []string, add, remove []string) error {
	_, err := m.SetMailboxesRemap(ctx, ids, add, remove)
	return err
}

// SetMailboxesRemap implements provider.Remapper.
//
// The source folder comes from the id itself, so remove is only advisory. A
// message ends up in exactly one folder on IMAP, so the first entry in add is
// the destination and any others are copies.
func (m *Mail) SetMailboxesRemap(ctx context.Context, ids, add, remove []string) ([]provider.Rename, error) {
	dest := ""
	if len(add) > 0 {
		dest = add[0]
	}
	if dest == "" {
		if len(remove) == 0 {
			return nil, nil
		}
		// Removing a message from its only folder is an archive.
		archive, err := m.roleRemote(ctx, model.RoleArchive)
		if err != nil {
			return nil, err
		}
		if archive == "" {
			return nil, fmt.Errorf("imap: this server has no Archive folder; " +
				"name one with archive_folder in [accounts.mail], or create one on the server")
		}
		dest = archive
	}
	return m.moveTo(ctx, ids, dest)
}

// Trash moves messages to the trash folder.
func (m *Mail) Trash(ctx context.Context, ids []string) error {
	_, err := m.TrashRemap(ctx, ids)
	return err
}

// TrashRemap implements provider.Remapper.
func (m *Mail) TrashRemap(ctx context.Context, ids []string) ([]provider.Rename, error) {
	trash, err := m.roleRemote(ctx, model.RoleTrash)
	if err != nil {
		return nil, err
	}
	if trash == "" {
		return nil, fmt.Errorf("imap: this server has no Trash folder; " +
			"name one with trash_folder in [accounts.mail]")
	}
	return m.moveTo(ctx, ids, trash)
}

// Restore moves messages back to the inbox, out of whatever folder they are
// in now.
func (m *Mail) Restore(ctx context.Context, ids []string) error {
	_, err := m.RestoreRemap(ctx, ids)
	return err
}

// RestoreRemap implements provider.Remapper.
func (m *Mail) RestoreRemap(ctx context.Context, ids []string) ([]provider.Rename, error) {
	inbox, err := m.roleRemote(ctx, model.RoleInbox)
	if err != nil {
		return nil, err
	}
	if inbox == "" {
		return nil, fmt.Errorf("imap: this account has no Inbox folder")
	}
	return m.moveTo(ctx, ids, inbox)
}

// moveTo moves every id into dest and reports the ids they now have.
func (m *Mail) moveTo(ctx context.Context, ids []string, dest string) ([]provider.Rename, error) {
	byMailbox, bad := groupRefs(ids)
	if len(bad) > 0 {
		m.log.Warn("skipping unparseable message ids", "count", len(bad))
	}

	var renames []provider.Rename
	for mailbox, refs := range byMailbox {
		if mailbox == dest {
			continue // already there
		}
		err := m.pool.with(ctx, mailbox, false, func(c *conn) error {
			set := uidSet(refs)

			if hasCap(c.caps, imapv2.CapMove) || hasCap(c.caps, imapv2.CapIMAP4rev2) {
				data, err := c.c.Move(set, dest).Wait()
				if err != nil {
					return c.fail(wrapErr("move to "+dest, err))
				}
				if data != nil {
					renames = append(renames, renamesFrom(mailbox, dest, refs,
						data.UIDValidity, data.SourceUIDs, data.DestUIDs)...)
				}
				return nil
			}

			// No MOVE: copy, then mark the originals deleted.
			data, err := c.c.Copy(set, dest).Wait()
			if err != nil {
				return c.fail(wrapErr("copy to "+dest, err))
			}
			if data != nil {
				renames = append(renames, renamesFrom(mailbox, dest, refs,
					data.UIDValidity, data.SourceUIDs, data.DestUIDs)...)
			}

			cmd := c.c.Store(set, &imapv2.StoreFlags{
				Op: imapv2.StoreFlagsAdd, Silent: true, Flags: []imapv2.Flag{imapv2.FlagDeleted},
			}, nil)
			if err := cmd.Close(); err != nil {
				return c.fail(wrapErr("flag deleted in "+mailbox, err))
			}

			if hasCap(c.caps, imapv2.CapUIDPlus) {
				if err := c.c.UIDExpunge(set).Close(); err != nil {
					return c.fail(wrapErr("expunge in "+mailbox, err))
				}
				return nil
			}
			// A bare EXPUNGE removes every \Deleted message in the folder,
			// including ones another client flagged and has not committed to.
			// Destroying somebody else's mail to tidy ours is not a trade worth
			// making: leave the tombstone.
			m.log.Warn("server has neither MOVE nor UIDPLUS; the source copy is flagged deleted but not expunged",
				"mailbox", mailbox, "count", len(refs))
			return nil
		})
		if err != nil {
			return renames, err
		}
	}
	return renames, nil
}

// renamesFrom pairs source uids with the uids the server gave the copies.
// Without UIDPLUS there is no COPYUID and no pairing to report; the next delta
// then discovers the move as a removal plus an addition.
func renamesFrom(src, dest string, refs []ref, destValidity uint32, srcSet, dstSet imapv2.NumSet) []provider.Rename {
	srcUIDs, ok1 := uidsOf(srcSet)
	dstUIDs, ok2 := uidsOf(dstSet)
	if !ok1 || !ok2 || len(srcUIDs) == 0 || len(srcUIDs) != len(dstUIDs) {
		return nil
	}
	// The source validity is the one the refs were minted under.
	var srcValidity uint32
	if len(refs) > 0 {
		srcValidity = refs[0].UIDValidity
	}
	out := make([]provider.Rename, 0, len(srcUIDs))
	for i := range srcUIDs {
		out = append(out, provider.Rename{
			Old: ref{Mailbox: src, UIDValidity: srcValidity, UID: srcUIDs[i]}.String(),
			New: ref{Mailbox: dest, UIDValidity: destValidity, UID: dstUIDs[i]}.String(),
		})
	}
	return out
}

// uidsOf enumerates a NumSet, but only when it really is a UID set with no
// dynamic "*" in it — pairing sources to destinations by position needs both
// sides fully enumerable.
func uidsOf(set imapv2.NumSet) ([]imapv2.UID, bool) {
	us, ok := set.(imapv2.UIDSet)
	if !ok || us.Dynamic() {
		return nil, false
	}
	return us.Nums()
}

// CreateDraft appends raw to the drafts folder.
func (m *Mail) CreateDraft(ctx context.Context, raw []byte) (string, error) {
	drafts, err := m.roleRemote(ctx, model.RoleDrafts)
	if err != nil {
		return "", err
	}
	if drafts == "" {
		return "", fmt.Errorf("imap: this server has no Drafts folder; " +
			"name one with drafts_folder in [accounts.mail]")
	}
	return m.append(ctx, drafts, raw, []imapv2.Flag{imapv2.FlagDraft, imapv2.FlagSeen}, time.Time{})
}

// append stores a message in a folder and returns the id it landed under.
func (m *Mail) append(ctx context.Context, mailbox string, raw []byte, flags []imapv2.Flag, when time.Time) (string, error) {
	var id string
	err := m.pool.withAny(ctx, func(c *conn) error {
		opts := &imapv2.AppendOptions{Flags: flags, Time: when}
		cmd := c.c.Append(mailbox, int64(len(raw)), opts)
		if _, err := cmd.Write(raw); err != nil {
			return c.fail(wrapErr("append to "+mailbox, err))
		}
		if err := cmd.Close(); err != nil {
			return c.fail(wrapErr("append to "+mailbox, err))
		}
		data, err := cmd.Wait()
		if err != nil {
			return c.fail(wrapErr("append to "+mailbox, err))
		}
		if data != nil && data.UID != 0 {
			id = ref{Mailbox: mailbox, UIDValidity: data.UIDValidity, UID: data.UID}.String()
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if id == "" {
		// No UIDPLUS, so no APPENDUID. The next delta will find it; the engine
		// only reports the id it is given.
		m.log.Debug("append returned no uid (no UIDPLUS)", "mailbox", mailbox)
	}
	return id, nil
}
