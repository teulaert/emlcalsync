package imap

import (
	"context"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// idleRefresh is how often the IDLE is renewed. RFC 2177 caps a server's
// patience at 29 minutes; renewing at 25 leaves room for a slow round trip.
const idleRefresh = 25 * time.Minute

// idleRetryMin and idleRetryMax bound the reconnect backoff.
const (
	idleRetryMin = 2 * time.Second
	idleRetryMax = 5 * time.Minute
)

// Watch implements provider.Pusher: it holds a connection in IDLE and reports
// when the server says something happened.
//
// IDLE watches the *selected* mailbox, so watching a whole account would cost
// one connection per folder — flatly incompatible with the connection budget
// servers actually allow. So this watches the inbox, and everything else rides
// the poll interval, which is cheap by design: a delta where nothing changed
// costs one round trip per folder and no SELECT.
//
// The connection is deliberately outside the pool. It is long-lived, it is
// always in a state where no other command may be sent, and it counts against
// the same per-account budget — which is why maxConns reserves room for it.
func (m *Mail) Watch(ctx context.Context, fn func(provider.ChangeHint)) error {
	backoff := idleRetryMin
	for {
		err := m.idleOnce(ctx, fn)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if IsAuth(err) {
			return err // a credential will not fix itself by waiting
		}
		m.log.Debug("idle dropped, reconnecting", "account", m.opts.Email, "err", err, "in", backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > idleRetryMax {
			backoff = idleRetryMax
		}
	}
}

// idleOnce holds one IDLE session until it drops or ctx ends.
func (m *Mail) idleOnce(ctx context.Context, fn func(provider.ChangeHint)) error {
	inbox, err := m.roleRemote(ctx, model.RoleInbox)
	if err != nil {
		return err
	}
	if inbox == "" {
		inbox = "INBOX"
	}

	notify := func() { fn(provider.ChangeHint{Mail: true}) }

	c, err := m.dialIdle(ctx, notify)
	if err != nil {
		return err
	}
	defer c.close()

	if !hasCap(c.caps, imapv2.CapIdle) {
		// Nothing to hold open. Report once so a watcher that just started does
		// not sit on stale data, then wait for the caller to give up on us.
		notify()
		<-ctx.Done()
		return ctx.Err()
	}

	if err := c.selectMailbox(ctx, inbox, true); err != nil {
		return err
	}

	for {
		cmd, err := c.c.Idle()
		if err != nil {
			return wrapErr("idle", err)
		}
		select {
		case <-ctx.Done():
			_ = cmd.Close()
			return ctx.Err()
		case <-time.After(idleRefresh):
			if err := cmd.Close(); err != nil {
				return wrapErr("idle", err)
			}
		}
	}
}

// dialIdle opens the dedicated push connection, wired so that unsolicited
// server chatter becomes a change hint.
func (m *Mail) dialIdle(ctx context.Context, notify func()) (*conn, error) {
	handler := &imapclient.UnilateralDataHandler{
		Expunge: func(uint32) { notify() },
		Mailbox: func(*imapclient.UnilateralDataMailbox) { notify() },
		Fetch:   func(*imapclient.FetchMessageData) { notify() },
	}
	return m.dialWith(ctx, handler)
}
