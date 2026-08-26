package jmap

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/teulaert/emlcalsync/internal/provider"
)

// Push tuning.
const (
	pushBackoffMin = 1 * time.Second
	pushBackoffMax = 60 * time.Second
	// pushPingSeconds asks the server for a keep-alive comment every 5 minutes
	// so a dead connection is noticed instead of hanging forever.
	pushPingSeconds = 300
	// pushReadTimeout must comfortably exceed pushPingSeconds.
	pushReadTimeout = 2 * pushPingSeconds * time.Second
)

// pushTypes are the JMAP data types we ask the server to notify us about.
var pushTypes = []string{"Email", "Mailbox", "CalendarEvent"}

// stateChange is the SSE payload (RFC 8620 §7.1).
type stateChange struct {
	Type string `json:"@type"`
	// Changed maps account id → data type → new state string.
	Changed map[string]map[string]string `json:"changed"`
}

var _ provider.Pusher = (*Client)(nil)

// Watch subscribes to the account's EventSource stream and calls fn whenever
// the server reports new state for a type we care about.
//
// It blocks until ctx is done (returning ctx.Err()) or authentication fails
// permanently (401). Every other failure — a 403 on the stream included — is
// retried with exponential backoff between 1s and 60s.
func (c *Client) Watch(ctx context.Context, fn func(provider.ChangeHint)) error {
	if fn == nil {
		return errors.New("jmap: Watch needs a callback")
	}
	backoff := c.pushBackoffMin
	lastEventID := ""
	// Remembering the last state per account/type keeps a reconnect (which
	// replays the current state) from triggering a redundant sync pass.
	seen := map[string]string{}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		gotData, newLastID, err := c.streamOnce(ctx, lastEventID, seen, fn)
		if newLastID != "" {
			lastEventID = newLastID
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var authErr *AuthError
		if errors.As(err, &authErr) {
			return err
		}
		if gotData {
			// The connection worked; treat the drop as transient.
			backoff = c.pushBackoffMin
		}
		if err != nil {
			c.log.Debug("jmap push stream ended, reconnecting", "backoff", backoff, "err", err)
		}
		if err := sleepCtx(ctx, jitter(backoff)); err != nil {
			return err
		}
		if backoff < c.pushBackoffMax {
			backoff = min(backoff*2, c.pushBackoffMax)
		}
	}
}

// streamOnce opens the EventSource connection and reads it until it fails.
func (c *Client) streamOnce(ctx context.Context, lastEventID string, seen map[string]string, fn func(provider.ChangeHint)) (gotData bool, newLastID string, err error) {
	s, err := c.Session(ctx)
	if err != nil {
		return false, "", err
	}
	if s.EventSourceURL == "" {
		return false, "", errors.New("jmap: session has no eventSourceUrl (server does not support push)")
	}
	url := buildEventSourceURL(s.EventSourceURL)

	// A stalled TCP connection would otherwise hang here forever. The server
	// pings every pushPingSeconds, so silence for twice that means it is gone.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	activity := make(chan struct{}, 1)
	go watchdog(streamCtx, cancel, activity, pushReadTimeout)

	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, url, nil)
	if err != nil {
		return false, "", err
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return false, "", err
	}

	c.log.Debug("jmap push stream open", "url", s.EventSourceURL)
	return c.readEvents(streamCtx, resp.Body, lastEventID, seen, fn, activity)
}

// buildEventSourceURL expands the eventSourceUrl template. RFC 8620 defines it
// with {types}, {closeafter} and {ping} variables; if a server hands back a
// plain URL instead, append the parameters so push still works.
func buildEventSourceURL(tmpl string) string {
	types := strings.Join(pushTypes, ",")
	ping := fmt.Sprint(pushPingSeconds)
	if strings.Contains(tmpl, "{types}") {
		return expandTemplate(tmpl, map[string]string{
			"types":      types,
			"closeafter": "no",
			"ping":       ping,
		})
	}
	sep := "?"
	if strings.Contains(tmpl, "?") {
		sep = "&"
	}
	return tmpl + sep + "types=" + escapeTemplateValue(types) + "&closeafter=no&ping=" + ping
}

// readEvents parses the SSE stream (WHATWG event-stream format) and dispatches
// StateChange payloads.
func (c *Client) readEvents(ctx context.Context, body io.Reader, lastEventID string, seen map[string]string, fn func(provider.ChangeHint), activity chan<- struct{}) (bool, string, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)

	var (
		eventName string
		data      strings.Builder
		gotData   bool
	)
	dispatch := func() {
		payload := data.String()
		data.Reset()
		name := eventName
		eventName = ""
		if payload == "" {
			return
		}
		// Fastmail sends "event: state"; be tolerant of servers that use the
		// default "message" event name.
		if name != "" && name != "state" && name != "message" {
			c.log.Debug("jmap push: ignoring event", "event", name)
			return
		}
		hint, ok := parseStateChange(payload, seen)
		if !ok {
			return
		}
		gotData = true
		if hint.Mail || hint.Calendar {
			fn(hint)
		}
	}

	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return gotData, lastEventID, err
		}
		if activity != nil {
			select {
			case activity <- struct{}{}:
			default:
			}
		}
		line := strings.TrimSuffix(sc.Text(), "\r")
		switch {
		case line == "":
			dispatch()
		case strings.HasPrefix(line, ":"):
			// Comment / keep-alive ping.
			gotData = true
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				eventName = value
			case "data":
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(value)
			case "id":
				lastEventID = value
			case "retry":
				// Server-suggested reconnect delay; our own backoff covers it.
			}
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	return gotData, lastEventID, err
}

// parseStateChange turns one StateChange payload into a ChangeHint, reporting
// only types whose state actually moved.
//
// RFC 8620 §7.1 tags every push payload with an @type, and the stream carries
// more than one kind (a PushSubscription verification, for one). Anything that
// is not a StateChange is ignored rather than mined for a "changed" map.
func parseStateChange(payload string, seen map[string]string) (provider.ChangeHint, bool) {
	var sc stateChange
	if err := json.Unmarshal([]byte(payload), &sc); err != nil {
		return provider.ChangeHint{}, false
	}
	if sc.Type != "StateChange" {
		return provider.ChangeHint{}, false
	}
	if len(sc.Changed) == 0 {
		return provider.ChangeHint{}, false
	}
	var hint provider.ChangeHint
	for accountID, types := range sc.Changed {
		for typ, state := range types {
			key := accountID + "/" + typ
			if prev, ok := seen[key]; ok && prev == state {
				continue
			}
			seen[key] = state
			switch typ {
			case "Email", "Mailbox", "Thread", "EmailDelivery", "EmailSubmission":
				hint.Mail = true
			case "CalendarEvent", "Calendar":
				hint.Calendar = true
			}
		}
	}
	return hint, true
}

// watchdog cancels the stream when no bytes have arrived for timeout.
func watchdog(ctx context.Context, cancel context.CancelFunc, activity <-chan struct{}, timeout time.Duration) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-activity:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(timeout)
		case <-t.C:
			cancel()
			return
		}
	}
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// 75%–125% of d, so many clients do not reconnect in lockstep.
	return d*3/4 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
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
