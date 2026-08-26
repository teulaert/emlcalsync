package jmap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/provider"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startWatch runs Watch in the background and returns the hint channel.
func startWatch(t *testing.T, c *Client) (<-chan provider.ChangeHint, context.CancelFunc, <-chan error) {
	t.Helper()
	c.pushBackoffMin = time.Millisecond
	c.pushBackoffMax = 5 * time.Millisecond

	hints := make(chan provider.ChangeHint, 32)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- c.Watch(ctx, func(h provider.ChangeHint) {
			select {
			case hints <- h:
			default:
			}
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Watch did not return after cancellation")
		}
	})
	return hints, cancel, done
}

func recvHint(t *testing.T, hints <-chan provider.ChangeHint) provider.ChangeHint {
	t.Helper()
	select {
	case h := <-hints:
		return h
	case <-time.After(5 * time.Second):
		t.Fatal("no change hint arrived")
		return provider.ChangeHint{}
	}
}

func TestWatchDeliversStateChanges(t *testing.T) {
	f := newFakeServer(t)
	c := f.client(t)
	// Watch needs the session, which it fetches itself.
	hints, _, _ := startWatch(t, c)

	waitFor(t, "the EventSource connection", func() bool { return f.sseConnections() >= 1 })

	f.pushStateChange(map[string]string{"Email": "email-1"})
	h := recvHint(t, hints)
	if !h.Mail || h.Calendar {
		t.Errorf("Email state change gave %+v, want Mail only", h)
	}

	f.pushStateChange(map[string]string{"CalendarEvent": "cal-1"})
	h = recvHint(t, hints)
	if h.Mail || !h.Calendar {
		t.Errorf("CalendarEvent state change gave %+v, want Calendar only", h)
	}

	f.pushStateChange(map[string]string{"Mailbox": "mailbox-1", "CalendarEvent": "cal-2"})
	h = recvHint(t, hints)
	if !h.Mail || !h.Calendar {
		t.Errorf("combined state change gave %+v, want both", h)
	}
}

func TestWatchIgnoresRepeatedStates(t *testing.T) {
	f := newFakeServer(t)
	c := f.client(t)
	hints, _, _ := startWatch(t, c)
	waitFor(t, "the EventSource connection", func() bool { return f.sseConnections() >= 1 })

	f.pushStateChange(map[string]string{"Email": "email-1"})
	recvHint(t, hints)

	// The identical state must not trigger a second sync pass.
	f.pushStateChange(map[string]string{"Email": "email-1"})
	// Then a genuinely new one, so we can tell the difference.
	f.pushStateChange(map[string]string{"Email": "email-2"})

	h := recvHint(t, hints)
	if !h.Mail {
		t.Fatalf("expected the new state to be reported, got %+v", h)
	}
	select {
	case extra := <-hints:
		t.Errorf("a repeated state produced an extra hint: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatchReconnects(t *testing.T) {
	f := newFakeServer(t)
	c := f.client(t)
	hints, _, _ := startWatch(t, c)
	waitFor(t, "the first EventSource connection", func() bool { return f.sseConnections() == 1 })

	f.pushStateChange(map[string]string{"Email": "email-1"})
	recvHint(t, hints)

	f.dropSSE()
	waitFor(t, "a reconnect", func() bool { return f.sseConnections() >= 2 })

	// The stream must work again after reconnecting.
	f.pushStateChange(map[string]string{"Email": "email-2"})
	if h := recvHint(t, hints); !h.Mail {
		t.Errorf("hint after reconnect = %+v", h)
	}
}

func TestWatchStopsOnContextCancel(t *testing.T) {
	f := newFakeServer(t)
	c := f.client(t)
	c.pushBackoffMin = time.Millisecond
	c.pushBackoffMax = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Watch(ctx, func(provider.ChangeHint) {}) }()

	waitFor(t, "the EventSource connection", func() bool { return f.sseConnections() >= 1 })
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Watch returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return after cancellation")
	}
}

func TestWatchStopsOnAuthFailure(t *testing.T) {
	f := newFakeServer(t)
	c, err := New(Options{Token: "wrong", SessionURL: f.srv.URL + "/jmap/session"})
	if err != nil {
		t.Fatal(err)
	}
	c.retryBase = time.Millisecond
	c.pushBackoffMin = time.Millisecond
	c.pushBackoffMax = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = c.Watch(ctx, func(provider.ChangeHint) {})
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("Watch returned %v, want an *AuthError rather than retrying forever", err)
	}
}

// TestWatchRetriesForbidden: a 403 on the stream is not a dead token, so Watch
// must reconnect rather than give up on the account for good.
func TestWatchRetriesForbidden(t *testing.T) {
	f := newFakeServer(t)
	f.failSSE = []int{403, 403}
	c := f.client(t)
	c.pushBackoffMin = time.Millisecond
	c.pushBackoffMax = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Watch(ctx, func(provider.ChangeHint) {}) }()

	deadline := time.Now().Add(5 * time.Second)
	for f.sseConnections() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Watch never got past the 403s")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Watch returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return after cancellation")
	}
}

func TestWatchNeedsCallback(t *testing.T) {
	f := newFakeServer(t)
	if err := f.client(t).Watch(context.Background(), nil); err == nil {
		t.Fatal("Watch with a nil callback should fail")
	}
}

// ---------------------------------------------------------------------------
// SSE / URL unit tests

func TestBuildEventSourceURL(t *testing.T) {
	got := buildEventSourceURL("https://api.example.com/jmap/event/{types}/{closeafter}/{ping}")
	want := "https://api.example.com/jmap/event/Email%2CMailbox%2CCalendarEvent/no/300"
	if got != want {
		t.Errorf("template form:\ngot  %q\nwant %q", got, want)
	}

	got = buildEventSourceURL("https://api.example.com/jmap/event?types={types}&closeafter={closeafter}&ping={ping}")
	if !strings.Contains(got, "types=Email%2CMailbox%2CCalendarEvent") ||
		!strings.Contains(got, "closeafter=no") || !strings.Contains(got, "ping=300") {
		t.Errorf("query template form = %q", got)
	}

	// A server that hands back a plain URL still has to work.
	got = buildEventSourceURL("https://api.example.com/jmap/event")
	if got != "https://api.example.com/jmap/event?types=Email%2CMailbox%2CCalendarEvent&closeafter=no&ping=300" {
		t.Errorf("plain URL form = %q", got)
	}
	got = buildEventSourceURL("https://api.example.com/jmap/event?x=1")
	if !strings.Contains(got, "?x=1&types=") {
		t.Errorf("plain URL with a query = %q", got)
	}
}

func TestParseStateChange(t *testing.T) {
	seen := map[string]string{}
	payload := `{"@type":"StateChange","changed":{"a1":{"Email":"e1","Mailbox":"m1"},"a2":{"CalendarEvent":"c1"}}}`

	h, ok := parseStateChange(payload, seen)
	if !ok {
		t.Fatal("payload should have parsed")
	}
	if !h.Mail || !h.Calendar {
		t.Errorf("hint = %+v", h)
	}
	if len(seen) != 3 {
		t.Errorf("seen states = %v", seen)
	}

	// Replaying it reports nothing new.
	h, ok = parseStateChange(payload, seen)
	if !ok {
		t.Fatal("replayed payload should still parse")
	}
	if h.Mail || h.Calendar {
		t.Errorf("replay produced %+v, want an empty hint", h)
	}

	// Types we do not care about are ignored.
	h, _ = parseStateChange(`{"@type":"StateChange","changed":{"a1":{"Contact":"x"}}}`, seen)
	if h.Mail || h.Calendar {
		t.Errorf("unrelated type produced %+v", h)
	}

	if _, ok := parseStateChange("not json", seen); ok {
		t.Error("garbage should not parse")
	}
	if _, ok := parseStateChange(`{"@type":"StateChange"}`, seen); ok {
		t.Error("a payload with no changes should be ignored")
	}

	// Only StateChange payloads are ours; anything else on the stream (a
	// PushSubscription verification, a future payload type) is ignored even
	// when it happens to carry a "changed" map.
	fresh := map[string]string{}
	if _, ok := parseStateChange(
		`{"@type":"PushVerification","changed":{"a1":{"Email":"z"}}}`, fresh); ok {
		t.Error("a non-StateChange payload should be ignored")
	}
	if len(fresh) != 0 {
		t.Errorf("a non-StateChange payload updated the seen states: %v", fresh)
	}
	if _, ok := parseStateChange(`{"changed":{"a1":{"Email":"z"}}}`, fresh); ok {
		t.Error("a payload without @type should be ignored")
	}
}

func TestReadEventsParsesSSEFraming(t *testing.T) {
	f := newFakeServer(t)
	c := f.client(t)

	stream := strings.Join([]string{
		": keep-alive comment",
		"",
		"event: state",
		"id: 42",
		`data: {"@type":"StateChange","changed":{"a1":`,
		`data: {"Email":"e1"}}}`,
		"",
		"event: something-else",
		`data: {"@type":"StateChange","changed":{"a1":{"Email":"e2"}}}`,
		"",
		"retry: 5000",
		"",
	}, "\n")

	var got []provider.ChangeHint
	gotData, lastID, err := c.readEvents(context.Background(), strings.NewReader(stream),
		"", map[string]string{}, func(h provider.ChangeHint) { got = append(got, h) }, nil)
	if err == nil {
		t.Fatal("reading to the end of the stream should report EOF")
	}
	if !gotData {
		t.Error("gotData should be true")
	}
	if lastID != "42" {
		t.Errorf("last event id = %q", lastID)
	}
	if len(got) != 1 {
		t.Fatalf("got %d hints, want 1 (multi-line data joined, unknown event names ignored)", len(got))
	}
	if !got[0].Mail {
		t.Errorf("hint = %+v", got[0])
	}
}

func TestJitterStaysInRange(t *testing.T) {
	for range 100 {
		d := jitter(time.Second)
		if d < 750*time.Millisecond || d > 1250*time.Millisecond {
			t.Fatalf("jitter(1s) = %v, outside 75%%-125%%", d)
		}
	}
	if jitter(0) != 0 {
		t.Error("jitter(0) should be 0")
	}
}

func TestClientImplementsPusher(t *testing.T) {
	var _ provider.Pusher = (*Client)(nil)
}
