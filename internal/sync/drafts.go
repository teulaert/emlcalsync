package sync

import (
	"context"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/store"
)

// trashSupersededDrafts clears away the drafts a sent reply made obsolete.
//
// The composer trashes the one draft it was finishing, but that is not the
// only one there can be. A reply drafted twice -- `mail draft` run again with
// a better text, which stores a second message beside the first because no
// provider updates a draft in place -- leaves the earlier version in Drafts,
// and once the later one is sent nothing points at it any more. It is still
// a live draft, so it sits in Drafts for good and `r` on the conversation
// reopens it.
//
// Every draft in the account that answers the message this send answers is
// one of those: the message has been answered now. Trash keeps them
// recoverable. The send has already gone out, so nothing here may fail it.
func (e *Engine) trashSupersededDrafts(ctx context.Context, acct config.Account, op Op, sent string) {
	p, err := mime.Parse(op.Raw)
	if err != nil || p.InReplyTo == "" {
		return
	}
	msgs, err := e.st.ListMessages(ctx, store.MessageFilter{
		Accounts:    []string{acct.Name},
		MailboxRole: "drafts", // excludes anything already in the trash
	})
	if err != nil {
		e.log.Warn("look for drafts the send superseded", "account", acct.Name, "err", err)
		return
	}
	var ids []string
	for _, m := range msgs {
		// The send may be the draft itself, adopted in place (JMAP answers
		// an import of bytes it already holds with that message's id).
		if m.RemoteID == sent || !m.Flags.Draft || m.InReplyTo != p.InReplyTo {
			continue
		}
		ids = append(ids, m.RemoteID)
	}
	if len(ids) == 0 {
		return
	}
	if _, err := e.Apply(ctx, acct.Name, Op{Kind: OpTrash, IDs: ids}); err != nil {
		e.log.Warn("trash drafts the send superseded", "account", acct.Name, "ids", ids, "err", err)
		return
	}
	e.log.Info("trashed drafts the send superseded", "account", acct.Name, "ids", ids)
}
