package imap

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strings"
	"sync"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"golang.org/x/sync/errgroup"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// FetchRaw downloads full messages, one folder at a time.
//
// BODY.PEEK[] rather than RFC822: the latter sets \Seen as a side effect, which
// on a first backfill would silently mark the user's entire archive read.
func (m *Mail) FetchRaw(ctx context.Context, ids []string, fn func(provider.RawMessage) error) error {
	byMailbox, bad := groupRefs(ids)
	if len(bad) > 0 {
		m.log.Warn("skipping unparseable message ids", "count", len(bad), "first", bad[0])
	}

	// fn must be called serially, so parallelism across folders is fine as long
	// as the callback is behind a lock.
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(m.maxConns())

	for mailbox, refs := range byMailbox {
		g.Go(func() error {
			return m.fetchRawFrom(gctx, mailbox, refs, &mu, fn)
		})
	}
	return g.Wait()
}

func (m *Mail) fetchRawFrom(ctx context.Context, mailbox string, refs []ref, mu *sync.Mutex, fn func(provider.RawMessage) error) error {
	section := &imapv2.FetchItemBodySection{Peek: true}
	return m.pool.with(ctx, mailbox, true, func(c *conn) error {
		if c.sel == nil {
			return nil
		}
		validity := c.sel.UIDValidity
		cmd := c.c.Fetch(uidSet(refs), &imapv2.FetchOptions{
			UID: true, Flags: true, InternalDate: true, RFC822Size: true,
			BodySection: []*imapv2.FetchItemBodySection{section},
		})
		defer cmd.Close()

		for {
			msg := cmd.Next()
			if msg == nil {
				break
			}
			buf, err := msg.Collect()
			if err != nil {
				return c.fail(wrapErr("fetch "+mailbox, err))
			}
			raw := bodyOf(buf)
			if raw == nil {
				// The message went away between the search and the fetch. The
				// engine reports ids it did not receive as gone, so silence is
				// the right answer.
				continue
			}
			rm := provider.RawMessage{
				Envelope: m.envelope(mailbox, validity, buf),
				Raw:      raw,
			}
			rm.Size = int64(len(raw))
			rm.ThreadID = threadIDFor(raw)

			mu.Lock()
			err = fn(rm)
			mu.Unlock()
			if err != nil {
				return err
			}
		}
		return c.fail(wrapErr("fetch "+mailbox, cmd.Close()))
	})
}

// bodyOf pulls the message bytes out of a fetch result.
func bodyOf(buf *imapclient.FetchMessageBuffer) []byte {
	for _, s := range buf.BodySection {
		if len(s.Bytes) > 0 {
			return s.Bytes
		}
	}
	return nil
}

// FetchEnvelopes refreshes flags without downloading bodies.
//
// The engine reaches for this through an optional interface during a reconcile;
// without it every reconcile leaves flags stale.
func (m *Mail) FetchEnvelopes(ctx context.Context, ids []string, fn func(provider.Envelope) error) error {
	byMailbox, _ := groupRefs(ids)
	for mailbox, refs := range byMailbox {
		err := m.pool.with(ctx, mailbox, true, func(c *conn) error {
			if c.sel == nil {
				return nil
			}
			msgs, err := c.c.Fetch(uidSet(refs), &imapv2.FetchOptions{
				UID: true, Flags: true, InternalDate: true, RFC822Size: true,
			}).Collect()
			if err != nil {
				return c.fail(wrapErr("fetch envelopes in "+mailbox, err))
			}
			for _, msg := range msgs {
				if err := fn(m.envelope(mailbox, c.sel.UIDValidity, msg)); err != nil {
					return err
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

// Total is the backfill's denominator. Approximate is fine: it feeds the
// percentage and the ETA and nothing else.
func (m *Mail) Total(ctx context.Context) (int, error) {
	m.mu.Lock()
	if m.haveTotal {
		n := m.total
		m.mu.Unlock()
		return n, nil
	}
	m.mu.Unlock()

	folders, err := m.syncedFolders(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, f := range folders {
		if f.Status != nil && f.Status.NumMessages != nil {
			total += int(*f.Status.NumMessages)
			continue
		}
		err := m.pool.withAny(ctx, func(c *conn) error {
			st, err := c.c.Status(f.Name, &imapv2.StatusOptions{NumMessages: true}).Wait()
			if err != nil {
				return c.fail(wrapErr("status "+f.Name, err))
			}
			if st.NumMessages != nil {
				total += int(*st.NumMessages)
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}

	m.mu.Lock()
	m.total, m.haveTotal = total, true
	m.mu.Unlock()
	return total, nil
}

// FetchAttachment has no IMAP equivalent: there is no server-side handle for a
// part, and emlcal archives whole messages anyway, so an attachment is always
// available from the blob. A stub row is the only way to reach here.
func (m *Mail) FetchAttachment(ctx context.Context, messageID, ref string) ([]byte, error) {
	return nil, fmt.Errorf("imap: attachments come from the archived message, not the server: %w", model.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Threading

// threadIDFor derives a conversation id from the message's own headers.
//
// This is a hint, not the last word: the store stitches threads properly over
// the whole account (it can see messages this provider never will, in folders
// it is not looking at). Supplying the root here means a message threads
// sensibly even before its neighbours arrive.
func threadIDFor(raw []byte) string {
	h := headerFields(raw, "references", "in-reply-to", "message-id")
	root := ""
	if refs := strings.Fields(h["references"]); len(refs) > 0 {
		root = refs[0]
	}
	if root == "" {
		root = strings.TrimSpace(h["in-reply-to"])
	}
	if root == "" {
		root = strings.TrimSpace(h["message-id"])
	}
	root = strings.Trim(strings.TrimSpace(root), "<>")
	if root == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(root))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return "r" + strings.ToLower(enc[:20])
}

// headerFields reads a few headers out of raw bytes without parsing the whole
// message. mime.Parse already runs over these bytes in the sync engine; doing
// it twice for three headers would be wasteful, and no provider imports
// internal/mime.
func headerFields(raw []byte, want ...string) map[string]string {
	wanted := make(map[string]bool, len(want))
	for _, w := range want {
		wanted[w] = true
	}
	out := make(map[string]string, len(want))

	head := raw
	if i := indexHeaderEnd(raw); i >= 0 {
		head = raw[:i]
	}

	var name, value string
	flush := func() {
		if name != "" && wanted[name] {
			out[name] = strings.TrimSpace(value)
		}
		name, value = "", ""
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n") {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if name != "" {
				value += " " + strings.TrimSpace(line)
			}
			continue
		}
		flush()
		n, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.ToLower(strings.TrimSpace(n))
		value = v
	}
	flush()
	return out
}

// indexHeaderEnd is the offset of the blank line ending the header block.
func indexHeaderEnd(raw []byte) int {
	if i := strings.Index(string(raw), "\r\n\r\n"); i >= 0 {
		return i
	}
	return strings.Index(string(raw), "\n\n")
}
