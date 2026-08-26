// Package jmap implements a hand-rolled JMAP client for Fastmail: mail
// (RFC 8621), calendars (draft-ietf-jmap-calendars / JSCalendar RFC 8984) and
// push (RFC 8620 EventSource).
//
// It deliberately depends only on the standard library. The three concrete
// types are:
//
//	*Client   protocol level: session, Request, Download, Upload, Watch
//	*Mail     provider.MailProvider     (c.Mail())
//	*Calendar provider.CalendarProvider (c.Calendar())
//
// References:
//
//	RFC 8620  JMAP core
//	RFC 8621  JMAP mail
//	RFC 8984  JSCalendar
//	draft-ietf-jmap-calendars-28  JMAP for calendars (still a draft; see calendar.go)
package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

// Capability URNs.
const (
	CapCore       = "urn:ietf:params:jmap:core"
	CapMail       = "urn:ietf:params:jmap:mail"
	CapSubmission = "urn:ietf:params:jmap:submission"
	CapCalendars  = "urn:ietf:params:jmap:calendars"
)

// DefaultSessionURL is Fastmail's JMAP session resource.
const DefaultSessionURL = "https://api.fastmail.com/jmap/session"

const (
	maxRetries       = 5
	defaultRetryBase = 500 * time.Millisecond
	maxRetryWait     = 30 * time.Second
)

// Options configures a Client.
type Options struct {
	// Token is the Fastmail API token (Settings → Privacy & Security → API
	// tokens) with at least the Mail and Calendars scopes. Required.
	Token string
	// Email is the account's own address. Used to pick the sending Identity
	// and to recognise "me" among calendar participants. Optional but
	// strongly recommended; when empty the client falls back to the session's
	// username.
	Email string
	// SessionURL defaults to DefaultSessionURL.
	SessionURL string
	// HTTPClient defaults to a client tuned for concurrent blob downloads and
	// long-lived EventSource streams (no overall timeout).
	HTTPClient *http.Client
	// UserAgent defaults to "emlcal-jmap/1".
	UserAgent string
	// Logger defaults to a no-op logger.
	Logger *slog.Logger
}

// Client is the protocol-level JMAP client. It is safe for concurrent use.
type Client struct {
	token      string
	email      string
	sessionURL string
	hc         *http.Client
	ua         string
	log        *slog.Logger

	// retryBase is the base backoff delay; overridden in tests.
	retryBase time.Duration
	// pushBackoffMin/Max bound the EventSource reconnect delay; overridden in
	// tests so a reconnect does not cost a real second.
	pushBackoffMin time.Duration
	pushBackoffMax time.Duration

	mu      sync.Mutex
	session *Session
	stale   bool
}

// New builds a Client. It does not perform any network I/O; the session is
// fetched lazily on first use.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.Token) == "" {
		return nil, errors.New("jmap: Options.Token is required")
	}
	c := &Client{
		token:          opts.Token,
		email:          strings.TrimSpace(opts.Email),
		sessionURL:     opts.SessionURL,
		hc:             opts.HTTPClient,
		ua:             opts.UserAgent,
		log:            opts.Logger,
		retryBase:      defaultRetryBase,
		pushBackoffMin: pushBackoffMin,
		pushBackoffMax: pushBackoffMax,
	}
	if c.sessionURL == "" {
		c.sessionURL = DefaultSessionURL
	}
	if c.ua == "" {
		c.ua = "emlcal-jmap/1"
	}
	if c.log == nil {
		c.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConnsPerHost = 16
		tr.ResponseHeaderTimeout = 60 * time.Second
		// No Client.Timeout: blob downloads and the EventSource stream are
		// long-lived. Callers bound work with the context instead.
		c.hc = &http.Client{Transport: tr}
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Session

// AccountInfo is one entry of the session's "accounts" map.
type AccountInfo struct {
	Name                string                     `json:"name"`
	IsPersonal          bool                       `json:"isPersonal"`
	IsReadOnly          bool                       `json:"isReadOnly"`
	AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
}

// CoreCapability is the value of the urn:ietf:params:jmap:core capability.
type CoreCapability struct {
	MaxSizeUpload         int64    `json:"maxSizeUpload"`
	MaxConcurrentUpload   int      `json:"maxConcurrentUpload"`
	MaxSizeRequest        int64    `json:"maxSizeRequest"`
	MaxConcurrentRequests int      `json:"maxConcurrentRequests"`
	MaxCallsInRequest     int      `json:"maxCallsInRequest"`
	MaxObjectsInGet       int      `json:"maxObjectsInGet"`
	MaxObjectsInSet       int      `json:"maxObjectsInSet"`
	CollationAlgorithms   []string `json:"collationAlgorithms"`
}

// Session is the JMAP session object (RFC 8620 §2).
type Session struct {
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	Accounts        map[string]AccountInfo     `json:"accounts"`
	PrimaryAccounts map[string]string          `json:"primaryAccounts"`
	Username        string                     `json:"username"`
	APIURL          string                     `json:"apiUrl"`
	DownloadURL     string                     `json:"downloadUrl"`
	UploadURL       string                     `json:"uploadUrl"`
	EventSourceURL  string                     `json:"eventSourceUrl"`
	State           string                     `json:"state"`

	// Core holds the parsed core capability with sane fallbacks applied.
	Core CoreCapability `json:"-"`
}

// HasCapability reports whether the server advertises a capability URN.
func (s *Session) HasCapability(urn string) bool {
	_, ok := s.Capabilities[urn]
	return ok
}

func (s *Session) fillDefaults() {
	if raw, ok := s.Capabilities[CapCore]; ok {
		_ = json.Unmarshal(raw, &s.Core)
	}
	// RFC 8620 defines no defaults for these; pick conservative values so we
	// never send an unbounded batch to a server that did not advertise limits.
	if s.Core.MaxObjectsInGet <= 0 {
		s.Core.MaxObjectsInGet = 500
	}
	if s.Core.MaxObjectsInSet <= 0 {
		s.Core.MaxObjectsInSet = 500
	}
	if s.Core.MaxCallsInRequest <= 0 {
		s.Core.MaxCallsInRequest = 16
	}
	if s.Core.MaxSizeUpload <= 0 {
		s.Core.MaxSizeUpload = 50 << 20
	}
}

// Session returns the cached session object, fetching it if it is missing or
// has been invalidated by a changed sessionState.
func (c *Client) Session(ctx context.Context) (*Session, error) {
	c.mu.Lock()
	s, stale := c.session, c.stale
	c.mu.Unlock()
	if s != nil && !stale {
		return s, nil
	}
	return c.RefreshSession(ctx)
}

// RefreshSession unconditionally re-fetches the session object.
func (c *Client) RefreshSession(ctx context.Context) (*Session, error) {
	resp, err := c.doRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sessionURL, nil)
		if err != nil {
			return nil, err
		}
		c.setHeaders(req)
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	var s Session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, fmt.Errorf("jmap: decoding session: %w", err)
	}
	if s.APIURL == "" {
		return nil, errors.New("jmap: session has no apiUrl")
	}
	s.fillDefaults()

	c.mu.Lock()
	c.session = &s
	c.stale = false
	if c.email == "" {
		c.email = s.Username
	}
	c.mu.Unlock()
	c.log.Debug("jmap session loaded", "apiUrl", s.APIURL, "state", s.State,
		"accounts", len(s.Accounts))
	return &s, nil
}

// AccountEmail returns the address used for Identity selection and for
// recognising the user among calendar participants.
func (c *Client) AccountEmail() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.email
}

// CalendarCapability resolves the capability URN this server publishes
// calendars under, together with the primary account id behind it.
//
// draft-ietf-jmap-calendars is not an RFC yet, so a server may still ship its
// calendar support under a vendor or legacy URN — Fastmail's own
// "https://www.fastmail.com/dev/calendars" predates the draft's spelling. The
// standard URN wins when present; otherwise any capability whose URN ends in
// ":calendars" is accepted, preferring one that names a primary account and
// falling back to an account that lists it among its accountCapabilities.
//
// The URN this returns must also be the one sent in "using" for calendar
// requests: a server will reject method calls for a capability the request did
// not claim.
func (c *Client) CalendarCapability(ctx context.Context) (urn, accountID string, err error) {
	s, err := c.Session(ctx)
	if err != nil {
		return "", "", err
	}
	if id := s.PrimaryAccounts[CapCalendars]; id != "" {
		return CapCalendars, id, nil
	}
	for _, u := range sortedKeys(s.PrimaryAccounts) {
		if u == CapCalendars || !isCalendarURN(u) {
			continue
		}
		if id := s.PrimaryAccounts[u]; id != "" {
			c.log.Info("jmap: using a non-standard calendars capability", "urn", u, "account", id)
			return u, id, nil
		}
	}
	// No primary account for any calendars URN: fall back to an account that
	// advertises one among its own capabilities.
	for _, u := range sortedKeys(s.Capabilities) {
		if !isCalendarURN(u) {
			continue
		}
		for _, acct := range sortedKeys(s.Accounts) {
			if _, ok := s.Accounts[acct].AccountCapabilities[u]; ok {
				c.log.Info("jmap: using a non-standard calendars capability",
					"urn", u, "account", acct, "via", "accountCapabilities")
				return u, acct, nil
			}
		}
	}
	return "", "", fmt.Errorf("jmap: no primary account for %s (token missing that scope?)", CapCalendars)
}

// isCalendarURN reports whether a capability URN is a calendars capability,
// standard or vendor-specific. Vendor spellings are both URN- and URL-shaped
// (Fastmail publishes "https://www.fastmail.com/dev/calendars"), so the last
// component is what is matched, after ':' or '/'.
func isCalendarURN(urn string) bool {
	return urn == CapCalendars ||
		strings.HasSuffix(urn, ":calendars") ||
		strings.HasSuffix(urn, "/calendars")
}

// PrimaryAccount returns the primary account id for a capability URN.
func (c *Client) PrimaryAccount(ctx context.Context, capability string) (string, error) {
	s, err := c.Session(ctx)
	if err != nil {
		return "", err
	}
	if id := s.PrimaryAccounts[capability]; id != "" {
		return id, nil
	}
	return "", fmt.Errorf("jmap: no primary account for %s (token missing that scope?)", capability)
}

// ---------------------------------------------------------------------------
// Request / Response

// Invocation is one method call: [name, arguments, callId].
type Invocation struct {
	Name string
	Args any
	ID   string
}

// MarshalJSON renders the invocation as the 3-element JSON array JMAP wants.
func (i Invocation) MarshalJSON() ([]byte, error) {
	args := i.Args
	if args == nil {
		args = map[string]any{}
	}
	return json.Marshal([]any{i.Name, args, i.ID})
}

// ResponseInvocation is one method response. Args is left raw so callers can
// decode it into a typed struct.
type ResponseInvocation struct {
	Name string
	Args json.RawMessage
	ID   string
}

// UnmarshalJSON parses the 3-element JSON array form.
func (r *ResponseInvocation) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 3 {
		return fmt.Errorf("jmap: method response has %d elements, want 3", len(raw))
	}
	if err := json.Unmarshal(raw[0], &r.Name); err != nil {
		return err
	}
	r.Args = raw[1]
	return json.Unmarshal(raw[2], &r.ID)
}

// Decode unmarshals the invocation arguments into dst.
func (r ResponseInvocation) Decode(dst any) error {
	if len(r.Args) == 0 {
		return fmt.Errorf("jmap: %s response has no arguments", r.Name)
	}
	if err := json.Unmarshal(r.Args, dst); err != nil {
		return fmt.Errorf("jmap: decoding %s response: %w", r.Name, err)
	}
	return nil
}

// Response is a parsed JMAP API response.
type Response struct {
	MethodResponses []ResponseInvocation `json:"methodResponses"`
	CreatedIDs      map[string]string    `json:"createdIds,omitempty"`
	SessionState    string               `json:"sessionState"`
}

// DecodeAt decodes the i'th method response into dst.
func (r *Response) DecodeAt(i int, dst any) error {
	if i < 0 || i >= len(r.MethodResponses) {
		return fmt.Errorf("jmap: no method response at index %d (have %d)", i, len(r.MethodResponses))
	}
	return r.MethodResponses[i].Decode(dst)
}

// Find returns the first response with the given call id.
func (r *Response) Find(callID string) (ResponseInvocation, bool) {
	for _, mr := range r.MethodResponses {
		if mr.ID == callID {
			return mr, true
		}
	}
	return ResponseInvocation{}, false
}

// ResultRef builds a JMAP result reference, used as a "#name" argument to
// chain one call's output into the next call's input.
//
//	args["#ids"] = ResultRef("q", "Email/query", "/ids")
func ResultRef(resultOf, name, path string) map[string]any {
	return map[string]any{"resultOf": resultOf, "name": name, "path": path}
}

// MethodError is a JMAP-level ["error", {...}] method response.
type MethodError struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	// Method is the name of the call that produced the error, CallID its id.
	Method string `json:"-"`
	CallID string `json:"-"`
}

func (e *MethodError) Error() string {
	s := "jmap: method error " + e.Type
	if e.Method != "" {
		s += " (call " + e.Method + "/" + e.CallID + ")"
	}
	if e.Description != "" {
		s += ": " + e.Description
	}
	return s
}

// IsMethodError reports whether err is a *MethodError with the given type.
func IsMethodError(err error, typ string) bool {
	var me *MethodError
	return errors.As(err, &me) && me.Type == typ
}

// AuthError is returned for HTTP 401 only. It is permanent: the credential is
// not accepted at all, so no amount of retrying or reconnecting will help.
//
// 403 deliberately does not land here: it means "authenticated, but not
// allowed to do this", which is a per-request condition (one calendar the
// token cannot see, one mailbox it may not write) rather than a dead token. It
// surfaces as a *RequestError so a single forbidden call cannot tear down a
// long-lived Watch.
type AuthError struct {
	Status int
	Body   string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("jmap: authentication failed (HTTP %d): %s", e.Status, e.Body)
}

// RequestError is a non-auth HTTP-level failure (e.g. a 400 problem+json).
type RequestError struct {
	Status int
	Type   string
	Detail string
	Body   string
}

func (e *RequestError) Error() string {
	msg := e.Detail
	if msg == "" {
		msg = e.Body
	}
	if e.Type != "" {
		return fmt.Sprintf("jmap: HTTP %d %s: %s", e.Status, e.Type, msg)
	}
	return fmt.Sprintf("jmap: HTTP %d: %s", e.Status, msg)
}

// Request performs one JMAP API request.
//
// CapCore is always added to using. The returned *Response is non-nil whenever
// the HTTP exchange succeeded, even if err is a *MethodError — err reports the
// first ["error", ...] method response so callers can inspect partial results
// when they care.
func (c *Client) Request(ctx context.Context, using []string, calls []Invocation) (*Response, error) {
	return c.request(ctx, using, calls, true)
}

// RequestNoRetry is Request for a batch that must not be retried: a
// non-idempotent write whose effect a retry could duplicate. See doOnce.
func (c *Client) RequestNoRetry(ctx context.Context, using []string, calls []Invocation) (*Response, error) {
	return c.request(ctx, using, calls, false)
}

func (c *Client) request(ctx context.Context, using []string, calls []Invocation, retry bool) (*Response, error) {
	if len(calls) == 0 {
		return nil, errors.New("jmap: Request with no method calls")
	}
	s, err := c.Session(ctx)
	if err != nil {
		return nil, err
	}
	if len(calls) > s.Core.MaxCallsInRequest {
		return nil, fmt.Errorf("jmap: %d calls exceeds server maxCallsInRequest=%d",
			len(calls), s.Core.MaxCallsInRequest)
	}

	body, err := json.Marshal(struct {
		Using       []string     `json:"using"`
		MethodCalls []Invocation `json:"methodCalls"`
	}{Using: withCore(using), MethodCalls: calls})
	if err != nil {
		return nil, fmt.Errorf("jmap: encoding request: %w", err)
	}

	httpResp, err := c.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.APIURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		c.setHeaders(req)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return req, nil
	}, retry)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	if err := checkStatus(httpResp); err != nil {
		return nil, err
	}

	var resp Response
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("jmap: decoding response: %w", err)
	}
	c.noteSessionState(resp.SessionState)

	for _, mr := range resp.MethodResponses {
		if mr.Name != "error" {
			continue
		}
		me := &MethodError{CallID: mr.ID}
		_ = json.Unmarshal(mr.Args, me)
		if me.Type == "" {
			me.Type = "unknownError"
		}
		me.Method = methodNameFor(calls, mr.ID)
		return &resp, me
	}
	return &resp, nil
}

// call is the common single-method convenience wrapper.
func (c *Client) call(ctx context.Context, using []string, name string, args map[string]any, dst any) error {
	return c.callWith(ctx, using, name, args, dst, true)
}

// callNoRetry is call for a non-idempotent method. See doOnce.
func (c *Client) callNoRetry(ctx context.Context, using []string, name string, args map[string]any, dst any) error {
	return c.callWith(ctx, using, name, args, dst, false)
}

func (c *Client) callWith(ctx context.Context, using []string, name string, args map[string]any, dst any, retry bool) error {
	resp, err := c.request(ctx, using, []Invocation{{Name: name, Args: args, ID: "0"}}, retry)
	if err != nil {
		return err
	}
	if dst == nil {
		return nil
	}
	return resp.DecodeAt(0, dst)
}

func methodNameFor(calls []Invocation, id string) string {
	for _, c := range calls {
		if c.ID == id {
			return c.Name
		}
	}
	return ""
}

func withCore(using []string) []string {
	out := make([]string, 0, len(using)+1)
	out = append(out, CapCore)
	for _, u := range using {
		if u != CapCore {
			out = append(out, u)
		}
	}
	return out
}

// noteSessionState invalidates the cached session when the server reports a
// new sessionState (RFC 8620 §2: any change to the session object).
func (c *Client) noteSessionState(state string) {
	if state == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil && c.session.State != state {
		c.stale = true
		c.log.Debug("jmap session state changed, will refresh",
			"old", c.session.State, "new", state)
	}
}

// ---------------------------------------------------------------------------
// Blobs

// Download fetches a blob through the session's downloadUrl template. name and
// typ are hints only; typ may be "" to let the server decide.
func (c *Client) Download(ctx context.Context, accountID, blobID, name, typ string) ([]byte, error) {
	s, err := c.Session(ctx)
	if err != nil {
		return nil, err
	}
	if s.DownloadURL == "" {
		return nil, errors.New("jmap: session has no downloadUrl")
	}
	if name == "" {
		name = "blob"
	}
	if typ == "" {
		typ = "application/octet-stream"
	}
	url := expandTemplate(s.DownloadURL, map[string]string{
		"accountId": accountID,
		"blobId":    blobID,
		"name":      name,
		"type":      typ,
	})

	resp, err := c.doRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		c.setHeaders(req)
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, fmt.Errorf("jmap: downloading blob %s: %w", blobID, err)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: reading blob %s: %v", model.ErrOffline, blobID, err)
	}
	return data, nil
}

// uploadResponse is the JSON body returned by the upload endpoint.
type uploadResponse struct {
	AccountID string `json:"accountId"`
	BlobID    string `json:"blobId"`
	Type      string `json:"type"`
	Size      int64  `json:"size"`
}

// Upload stores data as a temporary blob and returns its id.
func (c *Client) Upload(ctx context.Context, accountID, typ string, data []byte) (blobID string, size int64, err error) {
	s, err := c.Session(ctx)
	if err != nil {
		return "", 0, err
	}
	if s.UploadURL == "" {
		return "", 0, errors.New("jmap: session has no uploadUrl")
	}
	if int64(len(data)) > s.Core.MaxSizeUpload {
		return "", 0, fmt.Errorf("jmap: upload of %d bytes exceeds maxSizeUpload=%d",
			len(data), s.Core.MaxSizeUpload)
	}
	if typ == "" {
		typ = "application/octet-stream"
	}
	url := expandTemplate(s.UploadURL, map[string]string{"accountId": accountID})

	resp, err := c.doRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		c.setHeaders(req)
		req.Header.Set("Content-Type", typ)
		req.Header.Set("Accept", "application/json")
		req.ContentLength = int64(len(data))
		return req, nil
	})
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return "", 0, fmt.Errorf("jmap: upload: %w", err)
	}
	var ur uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&ur); err != nil {
		return "", 0, fmt.Errorf("jmap: decoding upload response: %w", err)
	}
	if ur.BlobID == "" {
		return "", 0, errors.New("jmap: upload response has no blobId")
	}
	if ur.Size == 0 {
		ur.Size = int64(len(data))
	}
	return ur.BlobID, ur.Size, nil
}

// ---------------------------------------------------------------------------
// HTTP plumbing

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.ua)
}

// doRetry runs mk() until it yields a response that is not retryable, retrying
// 429/503/5xx (honouring Retry-After) and transport failures. When retries are
// exhausted the error wraps model.ErrOffline so provider.IsOffline reports true.
func (c *Client) doRetry(ctx context.Context, mk func() (*http.Request, error)) (*http.Response, error) {
	return c.do(ctx, mk, true)
}

// doOnce runs mk() exactly once. It is for non-idempotent writes (Email/import,
// EmailSubmission/set): a 5xx or 429 says nothing about whether the server
// already applied the write, so retrying risks a duplicate import or, worse, a
// message sent twice. The failure is surfaced instead — transport errors as
// model.ErrOffline, HTTP errors through checkStatus.
func (c *Client) doOnce(ctx context.Context, mk func() (*http.Request, error)) (*http.Response, error) {
	return c.do(ctx, mk, false)
}

func (c *Client) do(ctx context.Context, mk func() (*http.Request, error), retry bool) (*http.Response, error) {
	if !retry {
		req, err := mk()
		if err != nil {
			return nil, err
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("%w: %v", model.ErrOffline, err)
		}
		return resp, nil
	}
	var (
		lastErr    error
		retryAfter time.Duration
	)
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.waitBackoff(ctx, attempt, retryAfter); err != nil {
				return nil, err
			}
			retryAfter = 0
		}
		req, err := mk()
		if err != nil {
			return nil, err
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			// Everything http.Client.Do returns here is transport level:
			// DNS failure, connection refused, TLS error, timeout.
			lastErr = err
			c.log.Debug("jmap transport error, retrying", "attempt", attempt+1, "err", err)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode >= 500 {
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
			snippet := readSnippet(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
			c.log.Debug("jmap server error, retrying", "attempt", attempt+1,
				"status", resp.StatusCode, "retryAfter", retryAfter)
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("%w: %v (after %d attempts)", model.ErrOffline, lastErr, maxRetries)
}

func (c *Client) waitBackoff(ctx context.Context, attempt int, retryAfter time.Duration) error {
	d := retryAfter
	if d <= 0 {
		d = c.retryBase << (attempt - 1)
		// Full jitter, so a fleet of clients does not synchronise.
		if d > 0 {
			d = time.Duration(rand.Int64N(int64(d))) + d/2
		}
	}
	if d > maxRetryWait {
		d = maxRetryWait
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// checkStatus turns a non-2xx response into a typed error. The body is
// consumed; callers must still close it.
func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body := readSnippet(resp.Body)
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return &AuthError{Status: resp.StatusCode, Body: body}
	case http.StatusNotFound:
		return fmt.Errorf("%w: HTTP 404: %s", model.ErrNotFound, body)
	}
	re := &RequestError{Status: resp.StatusCode, Body: body}
	// RFC 8620 §3.6.1: request-level errors use RFC 7807 problem details.
	var pd struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal([]byte(body), &pd) == nil {
		re.Type, re.Detail = pd.Type, pd.Detail
	}
	return re
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 2048))
	return strings.TrimSpace(string(b))
}

// expandTemplate performs RFC 6570 level-1 simple string expansion, which is
// all JMAP's URI templates use.
func expandTemplate(tmpl string, vars map[string]string) string {
	out := tmpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{"+k+"}", escapeTemplateValue(v))
	}
	return out
}

// escapeTemplateValue percent-encodes everything outside the RFC 3986
// unreserved set, as required for simple string expansion. net/url's escapers
// are not usable here: PathEscape leaves "&" and "=" alone, which would break
// a value substituted into a query string.
func escapeTemplateValue(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9',
			ch == '-', ch == '.', ch == '_', ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Small shared helpers

// jTime is a lenient JMAP UTCDate. Unparseable or null values decode to the
// zero time rather than failing the whole object.
type jTime struct{ time.Time }

func (t *jTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil || s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if v, err := time.Parse(layout, s); err == nil {
			t.Time = v
			return nil
		}
	}
	return nil
}

func (t jTime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.UTC().Format("2006-01-02T15:04:05Z"))
}

// trueKeys returns the keys of an Id[Boolean] map whose value is true, sorted
// so that output is deterministic.
func trueKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
