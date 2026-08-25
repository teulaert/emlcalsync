package gmail

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"
	gmailapi "google.golang.org/api/gmail/v1"

	"github.com/lennert/emlcal/internal/model"
)

// SetFlags adds/removes the label equivalents of the flags: UNREAD for
// unread, STARRED for flagged.
//
// Gmail has no equivalent of $answered, and DRAFT is owned by the server
// (drafts are created and destroyed through the drafts API), so those two
// flags are ignored here rather than silently mangling labels.
func (m *Mail) SetFlags(ctx context.Context, ids []string, set, clear model.Flags) error {
	var add, remove []string
	if set.Unread {
		add = append(add, "UNREAD")
	}
	if clear.Unread {
		remove = append(remove, "UNREAD")
	}
	if set.Flagged {
		add = append(add, "STARRED")
	}
	if clear.Flagged {
		remove = append(remove, "STARRED")
	}
	if set.Draft || clear.Draft || set.Answered || clear.Answered {
		m.log.Debug("ignoring flags Gmail cannot set", "draft", set.Draft || clear.Draft,
			"answered", set.Answered || clear.Answered)
	}
	return m.batchModify(ctx, ids, add, remove)
}

// SetMailboxes adds and removes labels by remote id.
func (m *Mail) SetMailboxes(ctx context.Context, ids []string, add, remove []string) error {
	return m.batchModify(ctx, ids, add, remove)
}

// batchModify applies label changes in chunks of maxModifyIDs.
func (m *Mail) batchModify(ctx context.Context, ids, add, remove []string) error {
	if len(ids) == 0 || (len(add) == 0 && len(remove) == 0) {
		return nil
	}
	for chunk := range chunks(ids, maxModifyIDs) {
		req := &gmailapi.BatchModifyMessagesRequest{
			Ids:            chunk,
			AddLabelIds:    add,
			RemoveLabelIds: remove,
		}
		err := m.do(ctx, "messages.batchModify", unitsBatchModify, func() error {
			return m.svc.Users.Messages.BatchModify(me, req).Context(ctx).Do()
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Trash moves messages to the trash. messages.trash is used rather than a
// label edit so Gmail applies its own semantics (removing the message from
// every label, keeping it restorable).
func (m *Mail) Trash(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(m.conc)
	for _, id := range ids {
		g.Go(func() error {
			err := m.do(gctx, "messages.trash", unitsMessagesTrash, func() error {
				_, err := m.svc.Users.Messages.Trash(me, id).Context(gctx).Do()
				return err
			})
			if err != nil && isNotFound(err) {
				m.log.Debug("gmail message already gone, not trashing", "id", id)
				return nil
			}
			return err
		})
	}
	return g.Wait()
}

// CreateDraft stores raw as a draft.
//
// It returns the id of the draft's *message*, so the id is comparable with
// every other remote id emlcal stores; the draft id itself is only needed to
// update or send the draft through the drafts API.
func (m *Mail) CreateDraft(ctx context.Context, raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("gmail: refusing to create an empty draft")
	}
	draft := &gmailapi.Draft{Message: &gmailapi.Message{Raw: encodeRaw(raw)}}
	var created *gmailapi.Draft
	err := m.do(ctx, "drafts.create", unitsDraftsCreate, func() error {
		var err error
		created, err = m.svc.Users.Drafts.Create(me, draft).Context(ctx).Do()
		return err
	})
	if err != nil {
		return "", err
	}
	if created.Message != nil && created.Message.Id != "" {
		return created.Message.Id, nil
	}
	return created.Id, nil
}

// Send submits raw. threadID, when set, attaches the message to an existing
// Gmail thread (the References/In-Reply-To/Subject headers must agree, which
// is the RFC 822 builder's job).
func (m *Mail) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("gmail: refusing to send an empty message")
	}
	msg := &gmailapi.Message{Raw: encodeRaw(raw), ThreadId: threadID}
	var sent *gmailapi.Message
	err := m.do(ctx, "messages.send", unitsMessagesSend, func() error {
		var err error
		sent, err = m.svc.Users.Messages.Send(me, msg).Context(ctx).Do()
		return err
	})
	if err != nil {
		return "", err
	}
	return sent.Id, nil
}

// FetchAttachment downloads one attachment body by its Gmail attachmentId.
func (m *Mail) FetchAttachment(ctx context.Context, messageID, ref string) ([]byte, error) {
	if messageID == "" || ref == "" {
		return nil, fmt.Errorf("gmail: attachment needs a message id and a reference")
	}
	var body *gmailapi.MessagePartBody
	err := m.do(ctx, "messages.attachments.get", unitsAttachmentGet, func() error {
		var err error
		body, err = m.svc.Users.Messages.Attachments.Get(me, messageID, ref).Context(ctx).Do()
		return err
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("gmail attachment %s of %s: %w", ref, messageID, model.ErrNotFound)
		}
		return nil, err
	}
	return decodeBase64URL(body.Data)
}

func encodeRaw(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}
