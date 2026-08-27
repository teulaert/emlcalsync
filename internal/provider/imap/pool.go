package imap

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
)

// idleTimeout is how long an unused connection is kept. Closing beats NOOPing:
// a server that rate-limits per account rewards holding none open between
// polls, and the long-lived connection is the push one, which is not pooled.
const idleTimeout = 2 * time.Minute

// conn is one authenticated connection, which in IMAP means one selected
// mailbox at a time.
type conn struct {
	c    *imapclient.Client
	caps imapv2.CapSet

	selected string // mailbox currently selected, "" for none
	readOnly bool
	sel      *imapv2.SelectData

	lastUse time.Time
	broken  bool
}

// pool hands out connections, preferring one that already has the wanted
// mailbox selected.
type pool struct {
	m   *Mail
	max int

	mu   sync.Mutex
	idle []*conn
	live int
	sem  chan struct{}
}

func newPool(m *Mail, max int) *pool {
	return &pool{m: m, max: max, sem: make(chan struct{}, max)}
}

// with runs fn on a connection with mailbox selected.
//
// SELECT is the expensive command — the server opens the folder's index — so
// affinity is the whole point of the pool: callers group work by folder and get
// back the connection that is already sitting in it.
func (p *pool) with(ctx context.Context, mailbox string, readOnly bool, fn func(*conn) error) error {
	return p.run(ctx, mailbox, readOnly, fn)
}

// withAny runs fn on any authenticated connection, for the commands that need
// no mailbox: LIST, STATUS, NAMESPACE, APPEND.
func (p *pool) withAny(ctx context.Context, fn func(*conn) error) error {
	return p.run(ctx, "", false, fn)
}

func (p *pool) run(ctx context.Context, mailbox string, readOnly bool, fn func(*conn) error) error {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.sem }()

	c, err := p.acquire(ctx, mailbox)
	if err != nil {
		return err
	}
	if mailbox != "" {
		if err := c.selectMailbox(ctx, mailbox, readOnly); err != nil {
			p.release(c)
			return err
		}
	}
	err = fn(c)
	p.release(c)
	return err
}

// acquire takes an idle connection, preferring one already on mailbox, or
// opens a new one.
func (p *pool) acquire(ctx context.Context, mailbox string) (*conn, error) {
	p.mu.Lock()
	// Exact match first, then anything.
	for i := len(p.idle) - 1; i >= 0; i-- {
		if p.idle[i].selected == mailbox {
			c := p.take(i)
			p.mu.Unlock()
			return c, nil
		}
	}
	for i := len(p.idle) - 1; i >= 0; i-- {
		if time.Since(p.idle[i].lastUse) < idleTimeout {
			c := p.take(i)
			p.mu.Unlock()
			return c, nil
		}
		c := p.take(i)
		go c.close()
	}
	p.live++
	p.mu.Unlock()

	c, err := p.m.dial(ctx)
	if err != nil {
		p.mu.Lock()
		p.live--
		p.mu.Unlock()
		return nil, err
	}
	return c, nil
}

// take removes idle[i]. Caller holds the lock.
func (p *pool) take(i int) *conn {
	c := p.idle[i]
	p.idle = append(p.idle[:i], p.idle[i+1:]...)
	return c
}

// release returns a connection, dropping it if it is no longer usable.
func (p *pool) release(c *conn) {
	p.mu.Lock()
	if c.broken {
		p.live--
		p.mu.Unlock()
		c.close()
		return
	}
	c.lastUse = time.Now()
	p.idle = append(p.idle, c)
	p.mu.Unlock()
}

// Close shuts every pooled connection.
func (p *pool) Close() error {
	p.mu.Lock()
	idle := p.idle
	p.idle = nil
	p.live = 0
	p.mu.Unlock()
	for _, c := range idle {
		c.close()
	}
	return nil
}

// dial opens and authenticates one pooled connection.
func (m *Mail) dial(ctx context.Context) (*conn, error) {
	return m.dialWith(ctx, nil)
}

// dialWith opens and authenticates a connection, optionally routing the
// server's unsolicited chatter somewhere (which is what push needs).
func (m *Mail) dialWith(ctx context.Context, handler *imapclient.UnilateralDataHandler) (*conn, error) {
	opts := &imapclient.Options{
		TLSConfig:             m.opts.TLSConfig,
		UnilateralDataHandler: handler,
	}

	var (
		cl  *imapclient.Client
		err error
	)
	switch m.security {
	case SecTLS:
		cl, err = imapclient.DialTLS(m.addr, opts)
	case SecStartTLS:
		cl, err = imapclient.DialStartTLS(m.addr, opts)
	case SecNone:
		cl, err = imapclient.DialInsecure(m.addr, opts)
	default:
		return nil, fmt.Errorf("imap: unknown security %q", m.security)
	}
	if err != nil {
		return nil, dialErr("connecting to "+m.addr, err)
	}

	c := &conn{c: cl, lastUse: time.Now()}
	if err := m.authenticate(ctx, c); err != nil {
		cl.Close()
		return nil, err
	}
	c.caps = cl.Caps()

	// Enable only what we actually handle. Any untagged response the client
	// cannot parse is a hard error that tears the connection down, so enabling
	// something speculatively is not free.
	if hasCap(c.caps, imapv2.CapCondStore) {
		if _, err := cl.Enable(imapv2.CapCondStore).Wait(); err != nil {
			m.log.Debug("CONDSTORE refused, carrying on without it", "err", err)
		}
	}
	return c, nil
}

// authenticate logs in, preferring SASL PLAIN where the server offers it.
func (m *Mail) authenticate(ctx context.Context, c *conn) error {
	caps := c.c.Caps()
	if hasCap(caps, imapv2.CapLoginDisabled) && !hasCap(caps, imapv2.Cap("AUTH=PLAIN")) {
		return m.authError("the server allows neither LOGIN nor AUTH=PLAIN on this connection")
	}

	var err error
	if hasCap(caps, imapv2.Cap("AUTH=PLAIN")) {
		err = c.c.Authenticate(sasl.NewPlainClient("", m.opts.User(), m.opts.Password))
	} else {
		err = c.c.Login(m.opts.User(), m.opts.Password).Wait()
	}
	if err == nil {
		return nil
	}
	if isAuthResponse(err) {
		return m.authError(errText(err))
	}
	return wrapErr("login", err)
}

// selectMailbox puts the connection on mailbox, reusing the current selection
// when it already matches and is no more restricted than asked.
func (c *conn) selectMailbox(ctx context.Context, mailbox string, readOnly bool) error {
	if c.selected == mailbox && (c.readOnly == readOnly || !readOnly) {
		return nil
	}
	opts := &imapv2.SelectOptions{
		ReadOnly:  readOnly,
		CondStore: hasCap(c.caps, imapv2.CapCondStore),
	}
	data, err := c.c.Select(mailbox, opts).Wait()
	if err != nil {
		c.selected, c.sel = "", nil
		if isTransport(err) {
			c.broken = true
		}
		return wrapErr("select "+mailbox, err)
	}
	c.selected, c.readOnly, c.sel = mailbox, readOnly, data
	return nil
}

// fail marks the connection unusable when the error was the transport.
func (c *conn) fail(err error) error {
	if err != nil && isTransport(err) {
		c.broken = true
	}
	return err
}

func (c *conn) close() {
	if c.c != nil {
		_ = c.c.Logout().Wait()
		_ = c.c.Close()
	}
}

// errText is the server's own words for an error, for use in a message the
// user reads.
func errText(err error) string {
	var se *imapv2.Error
	if errors.As(err, &se) && se.Text != "" {
		return se.Text
	}
	return err.Error()
}
