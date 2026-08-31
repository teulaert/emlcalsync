package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// seedEmails adds n messages one minute apart with predictable content.
func seedEmails(f *fakeServer, n int) []string {
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	ids := make([]string, 0, n)
	for i := range n {
		id := fmt.Sprintf("e%03d", i)
		f.addEmail(&fakeEmail{
			ID:         id,
			ThreadID:   fmt.Sprintf("t%03d", i/2),
			MailboxIDs: map[string]bool{"mb-inbox": true},
			Keywords:   map[string]bool{"$seen": true},
			ReceivedAt: base.Add(time.Duration(i) * time.Minute),
		}, []byte("From: a@example.com\r\nSubject: msg "+id+"\r\n\r\nbody\r\n"))
		ids = append(ids, id)
	}
	return ids
}

// ---------------------------------------------------------------------------
// Session

func TestSessionLoadAndRefresh(t *testing.T) {
	f := newFakeServer(t)
	c := f.client(t)
	ctx := testCtx(t)

	s, err := c.Session(ctx)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if s.APIURL != f.srv.URL+"/jmap/api" {
		t.Errorf("apiUrl = %q", s.APIURL)
	}
	if got := s.PrimaryAccounts[CapMail]; got != testAccount {
		t.Errorf("primary mail account = %q, want %q", got, testAccount)
	}
	if s.Core.MaxObjectsInGet != 50 || s.Core.MaxObjectsInSet != 20 {
		t.Errorf("core limits not parsed: %+v", s.Core)
	}
	if !s.HasCapability(CapCalendars) {
		t.Error("calendars capability missing")
	}
	if c.AccountEmail() != testEmail {
		t.Errorf("account email = %q", c.AccountEmail())
	}

	// Cached: no second fetch.
	if _, err := c.Session(ctx); err != nil {
		t.Fatal(err)
	}
	if f.sessionHits != 1 {
		t.Fatalf("session fetched %d times, want 1", f.sessionHits)
	}

	m := c.Mail()
	if _, err := m.Mailboxes(ctx); err != nil {
		t.Fatal(err)
	}
	if f.sessionHits != 1 {
		t.Fatalf("session refetched unnecessarily (%d)", f.sessionHits)
	}

	// A changed sessionState in a response must invalidate the cache.
	f.mu.Lock()
	f.sessionState = "session-1"
	f.mu.Unlock()
	if _, err := m.Mailboxes(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Session(ctx); err != nil {
		t.Fatal(err)
	}
	if f.sessionHits != 2 {
		t.Fatalf("session fetched %d times after state change, want 2", f.sessionHits)
	}
}

func TestNewRequiresToken(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New with no token should fail")
	}
	c, err := New(Options{Token: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if c.sessionURL != DefaultSessionURL {
		t.Errorf("default session URL = %q", c.sessionURL)
	}
}

// ---------------------------------------------------------------------------
// Mailboxes

func TestMailboxes(t *testing.T) {
	f := newFakeServer(t)
	m := f.client(t).Mail()

	boxes, err := m.Mailboxes(testCtx(t))
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	if len(boxes) != 7 {
		t.Fatalf("got %d mailboxes, want 7", len(boxes))
	}
	byID := map[string]model.Mailbox{}
	for _, b := range boxes {
		byID[b.RemoteID] = b
	}
	want := map[string]model.MailboxRole{
		"mb-inbox":   model.RoleInbox,
		"mb-archive": model.RoleArchive,
		"mb-sent":    model.RoleSent,
		"mb-drafts":  model.RoleDrafts,
		"mb-trash":   model.RoleTrash,
		"mb-junk":    model.RoleJunk,
		"mb-proj":    "",
	}
	for id, role := range want {
		if byID[id].Role != role {
			t.Errorf("mailbox %s role = %q, want %q", id, byID[id].Role, role)
		}
	}
	if byID["mb-proj"].ParentRemote != "mb-inbox" {
		t.Errorf("child parent = %q", byID["mb-proj"].ParentRemote)
	}
	if byID["mb-inbox"].ParentRemote != "" {
		t.Errorf("top-level mailbox has parent %q", byID["mb-inbox"].ParentRemote)
	}
	if byID["mb-inbox"].TotalCount != 3 || byID["mb-inbox"].UnreadCount != 1 {
		t.Errorf("inbox counts = %d/%d", byID["mb-inbox"].TotalCount, byID["mb-inbox"].UnreadCount)
	}
}

// ---------------------------------------------------------------------------
// State

func TestState(t *testing.T) {
	f := newFakeServer(t)
	m := f.client(t).Mail()

	got, err := m.State(testCtx(t))
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	want := `{"email":"email-0","mailbox":"mailbox-0"}`
	if got != want {
		t.Errorf("State = %q, want %q", got, want)
	}
	// It must round-trip through the parser.
	ms := parseMailState(got)
	if ms.Email != "email-0" || ms.Mailbox != "mailbox-0" {
		t.Errorf("parseMailState = %+v", ms)
	}
	// Legacy bare tokens are read as an Email state.
	if ms := parseMailState("email-9"); ms.Email != "email-9" || ms.Mailbox != "" {
		t.Errorf("legacy token parsed as %+v", ms)
	}
}

// ---------------------------------------------------------------------------
// Enumerate

func TestEnumeratePaging(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 25)
	m := f.client(t).Mail()
	ctx := testCtx(t)

	var (
		all    []provider.Envelope
		cursor string
		pages  int
	)
	for {
		page, next, err := m.Enumerate(ctx, cursor, 10)
		if err != nil {
			t.Fatalf("Enumerate(%q): %v", cursor, err)
		}
		pages++
		all = append(all, page...)
		if next == "" {
			break
		}
		if next == cursor {
			t.Fatalf("cursor did not advance past %q", cursor)
		}
		cursor = next
		if pages > 10 {
			t.Fatal("Enumerate did not terminate")
		}
	}
	if pages != 3 {
		t.Errorf("got %d pages, want 3", pages)
	}
	if len(all) != 25 {
		t.Fatalf("got %d envelopes, want 25", len(all))
	}
	// Newest first: a first sync archives this year's mail before 2005's.
	for i := 1; i < len(all); i++ {
		if all[i].Received.After(all[i-1].Received) {
			t.Fatalf("envelopes out of order at %d: %s after %s",
				i, all[i].RemoteID, all[i-1].RemoteID)
		}
	}
	first := all[0]
	if first.RemoteID != "e024" || first.ThreadID != "t012" {
		t.Errorf("first envelope = %+v, want the newest message e024", first)
	}
	if last := all[len(all)-1]; last.RemoteID != "e000" {
		t.Errorf("last envelope = %q, want the oldest message e000", last.RemoteID)
	}
	if q := f.captured("Email/query")[0]; !reflect.DeepEqual(q["sort"],
		[]any{map[string]any{"property": "receivedAt", "isAscending": false}}) {
		t.Errorf("sort = %#v, want receivedAt descending", q["sort"])
	}
	if first.Flags.Unread {
		t.Error("$seen keyword should clear Unread")
	}
	if !reflect.DeepEqual(first.Mailboxes, []string{"mb-inbox"}) {
		t.Errorf("mailboxes = %v", first.Mailboxes)
	}
	if first.Size == 0 {
		t.Error("size not carried through")
	}

	// The first page (and only the first) asks for a total.
	queries := f.captured("Email/query")
	if len(queries) != 3 {
		t.Fatalf("got %d Email/query calls", len(queries))
	}
	if queries[0]["calculateTotal"] != true {
		t.Error("first page should set calculateTotal")
	}
	if _, ok := queries[1]["calculateTotal"]; ok {
		t.Error("later pages should not set calculateTotal")
	}
	// Email/get must be chained by result reference, not a second round trip.
	if len(f.captured("Email/get")) != 3 {
		t.Errorf("expected one chained Email/get per page")
	}
}

// TestEnumerateSurvivesDeleteBehindCursor deletes messages that have already
// been enumerated. A position cursor would shift down by one per deletion and
// skip an unenumerated message; the anchor cursor must not. Enumeration runs
// newest first, so "behind the cursor" is the newest end of the mailbox.
func TestEnumerateSurvivesDeleteBehindCursor(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 25)
	m := f.client(t).Mail()
	ctx := testCtx(t)

	var (
		all    []provider.Envelope
		cursor string
		pages  int
	)
	for {
		page, next, err := m.Enumerate(ctx, cursor, 10)
		if err != nil {
			t.Fatalf("Enumerate(%q): %v", cursor, err)
		}
		pages++
		all = append(all, page...)
		if next == "" {
			break
		}
		cursor = next
		// Delete two messages from the page we have just consumed (e024 is the
		// newest message of all, e021 sits in the middle of that page), so the
		// whole list shifts under the cursor.
		if pages == 1 {
			f.deleteEmail("e024")
			f.deleteEmail("e021")
		}
		if pages > 10 {
			t.Fatal("Enumerate did not terminate")
		}
	}

	seen := map[string]bool{}
	for _, e := range all {
		if seen[e.RemoteID] {
			t.Fatalf("message %s enumerated twice", e.RemoteID)
		}
		seen[e.RemoteID] = true
	}
	// Everything except the two deleted messages must have been seen.
	for i := range 25 {
		id := fmt.Sprintf("e%03d", i)
		if id == "e024" || id == "e021" {
			continue
		}
		if !seen[id] {
			t.Errorf("message %s was skipped", id)
		}
	}
}

// TestEnumerateAnchorNotFound deletes the anchor itself: the server rejects the
// query and the client must fall back to the counted position instead of
// failing the enumeration.
func TestEnumerateAnchorNotFound(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 25)
	m := f.client(t).Mail()
	ctx := testCtx(t)

	page, cursor, err := m.Enumerate(ctx, "", 10)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(page) != 10 || cursor == "" {
		t.Fatalf("first page = %d envelopes, cursor %q", len(page), cursor)
	}
	var cur enumCursor
	if err := json.Unmarshal([]byte(cursor), &cur); err != nil {
		t.Fatalf("cursor %q is not JSON: %v", cursor, err)
	}
	if cur.Anchor != "e015" || cur.N != 10 || cur.Sort != sortDesc {
		t.Fatalf("cursor = %+v, want anchor e015, n 10 and a desc marker", cur)
	}

	// The anchor is destroyed before the next page is requested.
	f.deleteEmail("e015")
	f.resetCalls()

	page2, cursor2, err := m.Enumerate(ctx, cursor, 10)
	if err != nil {
		t.Fatalf("Enumerate after the anchor vanished: %v", err)
	}
	queries := f.captured("Email/query")
	if len(queries) != 2 {
		t.Fatalf("got %d Email/query calls, want the anchored one plus the position retry", len(queries))
	}
	if _, ok := queries[1]["anchor"]; ok {
		t.Error("the retry must not send an anchor")
	}
	if queries[1]["position"].(float64) != 9 {
		t.Errorf("retry position = %v, want 9 (one back, the anchor itself is gone)", queries[1]["position"])
	}
	if len(page2) != 10 || page2[0].RemoteID != "e014" {
		t.Fatalf("second page = %d envelopes starting at %q, want e014 (nothing skipped)",
			len(page2), page2[0].RemoteID)
	}
	if cursor2 == "" {
		t.Error("enumeration ended early")
	}
}

func TestEnumerateEmptyAccount(t *testing.T) {
	f := newFakeServer(t)
	m := f.client(t).Mail()
	page, next, err := m.Enumerate(testCtx(t), "", 50)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(page) != 0 || next != "" {
		t.Errorf("empty account gave page=%d next=%q", len(page), next)
	}
}

func TestEnumerateBadCursor(t *testing.T) {
	f := newFakeServer(t)
	m := f.client(t).Mail()
	for _, bad := range []string{"not-a-number", "-3", `{"n":-1}`, "{oops", `{"n":1,"sort":"sideways"}`} {
		if _, _, err := m.Enumerate(testCtx(t), bad, 10); err == nil {
			t.Errorf("cursor %q should have been rejected", bad)
		}
	}
}

// TestEnumerateLegacyPositionCursor: a backfill interrupted by an older build
// resumes from its numeric cursor instead of starting over.
func TestEnumerateLegacyPositionCursor(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 25)
	m := f.client(t).Mail()

	page, next, err := m.Enumerate(testCtx(t), "20", 10)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(page) != 5 || page[0].RemoteID != "e020" {
		t.Fatalf("page = %d envelopes starting at %q", len(page), page[0].RemoteID)
	}
	if next != "" {
		t.Errorf("a short page should end the enumeration, got cursor %q", next)
	}
}

func TestEnumerateClampsLimit(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 5)
	m := f.client(t).Mail()
	if _, _, err := m.Enumerate(testCtx(t), "", 10000); err != nil {
		t.Fatal(err)
	}
	q := f.captured("Email/query")[0]
	// maxObjectsInGet is 50 in the fake session.
	if q["limit"].(float64) != 50 {
		t.Errorf("limit = %v, want it clamped to the server maximum 50", q["limit"])
	}
}

// TestEnumerateLegacyAscendingCursorFinishesAscending: a cursor written before
// the direction was recorded belongs to a backfill that was walking the
// mailbox oldest-first. Re-sorting it mid-run would re-enumerate the newest
// mail and never reach the middle, so that run stays ascending — and keeps
// saying so in every cursor it writes.
func TestEnumerateLegacyAscendingCursorFinishesAscending(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 25)
	m := f.client(t).Mail()
	ctx := testCtx(t)

	page, next, err := m.Enumerate(ctx, `{"anchor":"e005","n":6}`, 10)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(page) != 10 || page[0].RemoteID != "e006" {
		t.Fatalf("page = %d envelopes starting at %q, want 10 starting at e006",
			len(page), page[0].RemoteID)
	}
	if q := f.captured("Email/query")[0]; !reflect.DeepEqual(q["sort"],
		[]any{map[string]any{"property": "receivedAt", "isAscending": true}}) {
		t.Errorf("sort = %#v, want receivedAt ascending for a legacy cursor", q["sort"])
	}
	var cur enumCursor
	if err := json.Unmarshal([]byte(next), &cur); err != nil {
		t.Fatalf("cursor %q is not JSON: %v", next, err)
	}
	if cur.Anchor != "e015" || cur.N != 16 || cur.Sort != sortAsc {
		t.Fatalf("cursor = %+v, want anchor e015, n 16 and an asc marker", cur)
	}

	// And the run really does finish oldest-to-newest from there.
	page2, _, err := m.Enumerate(ctx, next, 10)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if page2[0].RemoteID != "e016" {
		t.Errorf("second page starts at %q, want e016", page2[0].RemoteID)
	}
}

// ---------------------------------------------------------------------------
// Total

func TestTotal(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 7)
	m := f.client(t).Mail()
	ctx := testCtx(t)

	n, err := m.Total(ctx)
	if err != nil {
		t.Fatalf("Total: %v", err)
	}
	if n != 7 {
		t.Errorf("Total = %d, want 7", n)
	}
	queries := f.captured("Email/query")
	if len(queries) != 1 {
		t.Fatalf("got %d Email/query calls, want 1", len(queries))
	}
	if queries[0]["limit"].(float64) != 0 {
		t.Errorf("limit = %v, want 0: a count must not drag ids along", queries[0]["limit"])
	}
	if queries[0]["calculateTotal"] != true {
		t.Error("Total must ask for calculateTotal")
	}
	// No Email/get: the count is the whole point.
	if got := f.captured("Email/get"); len(got) != 0 {
		t.Errorf("Total issued %d Email/get calls", len(got))
	}

	// The answer is cached, so a backfill polling it costs nothing and its
	// denominator does not move while mail arrives.
	f.resetCalls()
	seedEmails(f, 9)
	again, err := m.Total(ctx)
	if err != nil {
		t.Fatalf("Total: %v", err)
	}
	if again != 7 {
		t.Errorf("second Total = %d, want the cached 7", again)
	}
	if len(f.captured("Email/query")) != 0 {
		t.Error("second Total went back to the server")
	}
}

// TestTotalComesFreeWithEnumerate: Enumerate's first page already asks for a
// total, so a backfill that enumerates and then wants a denominator makes no
// extra round trip.
func TestTotalComesFreeWithEnumerate(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 12)
	m := f.client(t).Mail()
	ctx := testCtx(t)

	if _, _, err := m.Enumerate(ctx, "", 5); err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	f.resetCalls()
	n, err := m.Total(ctx)
	if err != nil {
		t.Fatalf("Total: %v", err)
	}
	if n != 12 {
		t.Errorf("Total = %d, want 12", n)
	}
	if len(f.captured("Email/query")) != 0 {
		t.Error("Total re-queried what the first page already reported")
	}
}

// The engine reaches for the total through a structural interface; keep the
// shape it asserts on honest.
func TestMailSatisfiesTotaler(t *testing.T) {
	var mp provider.MailProvider = (&Client{}).Mail()
	if _, ok := mp.(interface {
		Total(ctx context.Context) (int, error)
	}); !ok {
		t.Fatal("*Mail no longer satisfies the Totaler shape the sync engine asserts on")
	}
}

// ---------------------------------------------------------------------------
// FetchEnvelopes

func TestFetchEnvelopes(t *testing.T) {
	f := newFakeServer(t)
	ids := seedEmails(f, 120) // maxObjectsInGet is 50 in the fake session
	m := f.client(t).Mail()

	// One id the server no longer knows, in the middle of a chunk.
	f.deleteEmail("e060")

	var got []provider.Envelope
	if err := m.FetchEnvelopes(testCtx(t), ids, func(env provider.Envelope) error {
		got = append(got, env)
		return nil
	}); err != nil {
		t.Fatalf("FetchEnvelopes: %v", err)
	}
	if len(got) != 119 {
		t.Fatalf("got %d envelopes, want 119 (the deleted one skipped)", len(got))
	}
	for _, env := range got {
		if env.RemoteID == "e060" {
			t.Fatal("a notFound id must not be reported")
		}
	}
	first := got[0]
	if first.RemoteID != "e000" || first.ThreadID != "t000" ||
		first.Flags.Unread || !reflect.DeepEqual(first.Mailboxes, []string{"mb-inbox"}) {
		t.Errorf("first envelope = %+v", first)
	}
	if first.Size == 0 || first.Received.IsZero() {
		t.Errorf("envelope is missing metadata: %+v", first)
	}

	gets := f.captured("Email/get")
	if len(gets) != 3 {
		t.Fatalf("got %d Email/get calls, want 3 chunks of at most 50", len(gets))
	}
	for i, g := range gets {
		props, _ := g["properties"].([]any)
		var names []string
		for _, p := range props {
			names = append(names, p.(string))
		}
		want := []string{"id", "threadId", "mailboxIds", "keywords", "receivedAt", "size"}
		if !reflect.DeepEqual(names, want) {
			t.Errorf("call %d properties = %v, want %v (no blobId: nothing is downloaded)", i, names, want)
		}
		if n := len(g["ids"].([]any)); n > 50 {
			t.Errorf("call %d asked for %d ids, over maxObjectsInGet", i, n)
		}
	}
}

func TestFetchEnvelopesEmpty(t *testing.T) {
	f := newFakeServer(t)
	m := f.client(t).Mail()
	if err := m.FetchEnvelopes(testCtx(t), nil, func(provider.Envelope) error {
		t.Fatal("fn called for an empty id list")
		return nil
	}); err != nil {
		t.Fatalf("FetchEnvelopes: %v", err)
	}
	if len(f.captured("Email/get")) != 0 {
		t.Error("an empty id list should not reach the server")
	}
}

func TestFetchEnvelopesCallbackErrorAborts(t *testing.T) {
	f := newFakeServer(t)
	ids := seedEmails(f, 120)
	m := f.client(t).Mail()

	boom := errors.New("boom")
	calls := 0
	err := m.FetchEnvelopes(testCtx(t), ids, func(provider.Envelope) error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("FetchEnvelopes returned %v, want the callback error", err)
	}
	if calls != 1 {
		t.Errorf("callback ran %d times after failing", calls)
	}
	if len(f.captured("Email/get")) != 1 {
		t.Error("the fetch kept going after the callback failed")
	}
}

// ---------------------------------------------------------------------------
// FetchRaw

func TestFetchRaw(t *testing.T) {
	f := newFakeServer(t)
	ids := seedEmails(f, 20)
	m := f.client(t).Mail()

	// Ask for every seeded id plus two that do not exist.
	want := append(append([]string{}, ids...), "ghost-1", "ghost-2")

	var (
		mu   sync.Mutex
		got  = map[string][]byte{}
		envs = map[string]provider.Envelope{}
	)
	err := m.FetchRaw(testCtx(t), want, func(rm provider.RawMessage) error {
		mu.Lock()
		defer mu.Unlock()
		if _, dup := got[rm.RemoteID]; dup {
			t.Errorf("message %s delivered twice", rm.RemoteID)
		}
		got[rm.RemoteID] = rm.Raw
		envs[rm.RemoteID] = rm.Envelope
		return nil
	})
	if err != nil {
		t.Fatalf("FetchRaw: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("got %d messages, want 20 (notFound ids must be skipped)", len(got))
	}
	if body := string(got["e005"]); !strings.Contains(body, "Subject: msg e005") {
		t.Errorf("raw message body = %q", body)
	}
	e := envs["e005"]
	if e.ThreadID == "" || e.Size == 0 || e.Received.IsZero() {
		t.Errorf("envelope not populated: %+v", e)
	}
}

func TestFetchRawCallbackErrorAborts(t *testing.T) {
	f := newFakeServer(t)
	ids := seedEmails(f, 30)
	m := f.client(t).Mail()

	sentinel := errors.New("stop right there")
	var mu sync.Mutex
	calls := 0
	err := m.FetchRaw(testCtx(t), ids, func(provider.RawMessage) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("FetchRaw error = %v, want %v", err, sentinel)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls == 0 || calls == 30 {
		t.Errorf("callback ran %d times; expected an early abort", calls)
	}
}

func TestFetchRawChunksGets(t *testing.T) {
	f := newFakeServer(t)
	ids := seedEmails(f, 120) // maxObjectsInGet is 50
	m := f.client(t).Mail()
	if err := m.FetchRaw(testCtx(t), ids, func(provider.RawMessage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n := len(f.captured("Email/get")); n != 3 {
		t.Errorf("Email/get called %d times, want 3 chunks of <=50", n)
	}
}

func TestFetchRawEmpty(t *testing.T) {
	f := newFakeServer(t)
	m := f.client(t).Mail()
	if err := m.FetchRaw(testCtx(t), nil, func(provider.RawMessage) error {
		t.Fatal("callback should not run")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Changes

func TestChanges(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 5)
	// Two rounds of Email/changes, exercising the hasMoreChanges loop.
	f.emailChanges["email-0"] = changeScript{
		Created: []string{"e000"}, Updated: []string{"e001"},
		NewState: "email-1", HasMore: true,
	}
	f.emailChanges["email-1"] = changeScript{
		Updated: []string{"e002"}, Destroyed: []string{"gone-1"},
		NewState: "email-2",
	}
	f.mailboxChanges["mailbox-0"] = changeScript{
		Updated: []string{"mb-inbox"}, NewState: "mailbox-1",
	}

	m := f.client(t).Mail()
	ch, err := m.Changes(testCtx(t), `{"email":"email-0","mailbox":"mailbox-0"}`)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(ch.Added) != 1 || ch.Added[0].RemoteID != "e000" {
		t.Errorf("Added = %+v", ch.Added)
	}
	if len(ch.Updated) != 2 {
		t.Fatalf("Updated = %+v", ch.Updated)
	}
	updIDs := []string{ch.Updated[0].RemoteID, ch.Updated[1].RemoteID}
	sort.Strings(updIDs)
	if !reflect.DeepEqual(updIDs, []string{"e001", "e002"}) {
		t.Errorf("updated ids = %v", updIDs)
	}
	// Updated envelopes must carry current flags and mailboxes.
	if ch.Updated[0].Mailboxes == nil {
		t.Error("updated envelope has no mailboxes")
	}
	if !reflect.DeepEqual(ch.Removed, []string{"gone-1"}) {
		t.Errorf("Removed = %v", ch.Removed)
	}
	if !ch.MailboxesChanged {
		t.Error("MailboxesChanged should be true")
	}
	if ch.NewState != `{"email":"email-2","mailbox":"mailbox-1"}` {
		t.Errorf("NewState = %q", ch.NewState)
	}
}

func TestChangesNoMailboxActivity(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 2)
	f.emailChanges["email-0"] = changeScript{Created: []string{"e000"}, NewState: "email-1"}

	m := f.client(t).Mail()
	ch, err := m.Changes(testCtx(t), `{"email":"email-0","mailbox":"mailbox-0"}`)
	if err != nil {
		t.Fatal(err)
	}
	if ch.MailboxesChanged {
		t.Error("MailboxesChanged should be false when nothing changed")
	}
}

func TestChangesDestroyedWins(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 3)
	// The same id is reported created and then destroyed in the window.
	f.emailChanges["email-0"] = changeScript{
		Created: []string{"e000", "e001"}, Updated: []string{"e001"},
		Destroyed: []string{"e000"}, NewState: "email-1",
	}
	m := f.client(t).Mail()
	ch, err := m.Changes(testCtx(t), `{"email":"email-0","mailbox":"mailbox-0"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Added) != 1 || ch.Added[0].RemoteID != "e001" {
		t.Errorf("Added = %+v, want only e001", ch.Added)
	}
	if len(ch.Updated) != 0 {
		t.Errorf("an id that is also created must not appear in Updated: %+v", ch.Updated)
	}
	if !reflect.DeepEqual(ch.Removed, []string{"e000"}) {
		t.Errorf("Removed = %v", ch.Removed)
	}
}

func TestChangesStateExpired(t *testing.T) {
	f := newFakeServer(t)
	f.emailChanges["email-0"] = changeScript{ErrorType: "cannotCalculateChanges"}
	m := f.client(t).Mail()

	_, err := m.Changes(testCtx(t), `{"email":"email-0","mailbox":"mailbox-0"}`)
	if !errors.Is(err, provider.ErrStateExpired) {
		t.Fatalf("Changes error = %v, want ErrStateExpired", err)
	}
}

func TestChangesEmptyStateExpired(t *testing.T) {
	f := newFakeServer(t)
	m := f.client(t).Mail()
	if _, err := m.Changes(testCtx(t), ""); !errors.Is(err, provider.ErrStateExpired) {
		t.Fatalf("empty state gave %v, want ErrStateExpired", err)
	}
}

func TestChangesMailboxStateExpiredDegrades(t *testing.T) {
	f := newFakeServer(t)
	f.emailChanges["email-0"] = changeScript{NewState: "email-1"}
	f.mailboxChanges["mailbox-0"] = changeScript{ErrorType: "cannotCalculateChanges"}

	m := f.client(t).Mail()
	ch, err := m.Changes(testCtx(t), `{"email":"email-0","mailbox":"mailbox-0"}`)
	if err != nil {
		t.Fatalf("an expired mailbox state must not fail the mail delta: %v", err)
	}
	if !ch.MailboxesChanged {
		t.Error("MailboxesChanged should be forced true")
	}
	if ch.NewState != `{"email":"email-1","mailbox":"mailbox-0"}` {
		t.Errorf("NewState = %q", ch.NewState)
	}
}

// TestChangesStuckStateExpired: the server keeps saying hasMoreChanges without
// advancing its state. Returning the partial delta would advance the sync
// engine past changes it never saw, so this must report ErrStateExpired.
func TestChangesStuckStateExpired(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 3)
	f.emailChanges["email-0"] = changeScript{
		Created: []string{"e000"}, NewState: "email-0", HasMore: true,
	}
	m := f.client(t).Mail()

	_, err := m.Changes(testCtx(t), `{"email":"email-0","mailbox":"mailbox-0"}`)
	if !errors.Is(err, provider.ErrStateExpired) {
		t.Fatalf("Changes error = %v, want ErrStateExpired", err)
	}
}

// TestChangesEmptyNewStateExpired covers the same guard when the server sends
// hasMoreChanges with no newState at all.
func TestChangesEmptyNewStateExpired(t *testing.T) {
	f := newFakeServer(t)
	f.emailChanges["email-0"] = changeScript{Created: []string{"e000"}, NewState: "", HasMore: true}
	m := f.client(t).Mail()

	_, err := m.Changes(testCtx(t), `{"email":"email-0","mailbox":"mailbox-0"}`)
	if !errors.Is(err, provider.ErrStateExpired) {
		t.Fatalf("Changes error = %v, want ErrStateExpired", err)
	}
}

// TestChangesLoopLimitStateExpired: a server whose state advances forever must
// hit the loop guard and report ErrStateExpired rather than spin.
func TestChangesLoopLimitStateExpired(t *testing.T) {
	f := newFakeServer(t)
	m := f.client(t).Mail()

	_, err := m.Changes(testCtx(t), `{"email":"spin-0","mailbox":"mailbox-0"}`)
	if !errors.Is(err, provider.ErrStateExpired) {
		t.Fatalf("Changes error = %v, want ErrStateExpired", err)
	}
	if n := len(f.captured("Email/changes")); n != changesLoopLimit {
		t.Errorf("made %d Email/changes calls, want the loop limit %d", n, changesLoopLimit)
	}
}

// TestChangesStuckMailboxDegrades: a Mailbox/changes loop that cannot be paged
// to the end must degrade to a mailbox resync, not fail the mail delta.
func TestChangesStuckMailboxDegrades(t *testing.T) {
	f := newFakeServer(t)
	f.emailChanges["email-0"] = changeScript{NewState: "email-1"}
	f.mailboxChanges["mailbox-0"] = changeScript{
		Updated: []string{"mb-inbox"}, NewState: "mailbox-0", HasMore: true,
	}
	m := f.client(t).Mail()

	ch, err := m.Changes(testCtx(t), `{"email":"email-0","mailbox":"mailbox-0"}`)
	if err != nil {
		t.Fatalf("a stuck mailbox delta must not fail the mail delta: %v", err)
	}
	if !ch.MailboxesChanged {
		t.Error("MailboxesChanged should be forced true")
	}
}

func TestChangesLegacyBareToken(t *testing.T) {
	f := newFakeServer(t)
	f.emailChanges["email-0"] = changeScript{NewState: "email-1"}
	m := f.client(t).Mail()

	ch, err := m.Changes(testCtx(t), "email-0")
	if err != nil {
		t.Fatal(err)
	}
	if !ch.MailboxesChanged {
		t.Error("a token without a mailbox state should force a mailbox resync")
	}
	if ch.NewState != `{"email":"email-1","mailbox":"mailbox-0"}` {
		t.Errorf("NewState = %q", ch.NewState)
	}
}

// ---------------------------------------------------------------------------
// Writes

func TestSetFlagsPatchBodies(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 1)
	f.email("e000").Keywords = map[string]bool{}
	m := f.client(t).Mail()
	ctx := testCtx(t)

	// Mark read and flagged.
	if err := m.SetFlags(ctx, []string{"e000"},
		model.Flags{Flagged: true}, model.Flags{Unread: true}); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	sets := f.captured("Email/set")
	if len(sets) != 1 {
		t.Fatalf("got %d Email/set calls", len(sets))
	}
	patch := sets[0]["update"].(map[string]any)["e000"].(map[string]any)
	want := map[string]any{"keywords/$seen": true, "keywords/$flagged": true}
	if !reflect.DeepEqual(patch, want) {
		t.Errorf("patch = %#v, want %#v", patch, want)
	}
	if kw := f.email("e000").Keywords; !kw["$seen"] || !kw["$flagged"] {
		t.Errorf("server keywords = %v", kw)
	}

	// Mark unread and unflag: $seen must be removed, not set to false.
	f.resetCalls()
	if err := m.SetFlags(ctx, []string{"e000"},
		model.Flags{Unread: true}, model.Flags{Flagged: true, Answered: true}); err != nil {
		t.Fatal(err)
	}
	patch = f.captured("Email/set")[0]["update"].(map[string]any)["e000"].(map[string]any)
	want = map[string]any{
		"keywords/$seen":     nil,
		"keywords/$flagged":  nil,
		"keywords/$answered": nil,
	}
	if !reflect.DeepEqual(patch, want) {
		t.Errorf("patch = %#v, want %#v", patch, want)
	}
	if kw := f.email("e000").Keywords; kw["$seen"] || kw["$flagged"] {
		t.Errorf("server keywords = %v", kw)
	}
}

func TestSetFlagsChunksBySetLimit(t *testing.T) {
	f := newFakeServer(t)
	ids := seedEmails(f, 45) // maxObjectsInSet is 20
	m := f.client(t).Mail()
	if err := m.SetFlags(testCtx(t), ids, model.Flags{Flagged: true}, model.Flags{}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.captured("Email/set")); n != 3 {
		t.Errorf("Email/set called %d times, want 3 chunks of <=20", n)
	}
}

func TestSetFlagsNoopDoesNothing(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 1)
	m := f.client(t).Mail()
	if err := m.SetFlags(testCtx(t), []string{"e000"}, model.Flags{}, model.Flags{}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.captured("Email/set")); n != 0 {
		t.Errorf("an empty flag change should not hit the network (%d calls)", n)
	}
}

func TestSetMailboxes(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 1)
	m := f.client(t).Mail()

	if err := m.SetMailboxes(testCtx(t), []string{"e000"},
		[]string{"mb-archive"}, []string{"mb-inbox"}); err != nil {
		t.Fatalf("SetMailboxes: %v", err)
	}
	patch := f.captured("Email/set")[0]["update"].(map[string]any)["e000"].(map[string]any)
	want := map[string]any{"mailboxIds/mb-archive": true, "mailboxIds/mb-inbox": nil}
	if !reflect.DeepEqual(patch, want) {
		t.Errorf("patch = %#v, want %#v", patch, want)
	}
	if mb := f.email("e000").MailboxIDs; !mb["mb-archive"] || mb["mb-inbox"] {
		t.Errorf("server mailboxIds = %v", mb)
	}
}

func TestSetMailboxesConflict(t *testing.T) {
	f := newFakeServer(t)
	m := f.client(t).Mail()
	err := m.SetMailboxes(testCtx(t), []string{"e000"}, []string{"mb-x"}, []string{"mb-x"})
	if err == nil || !strings.Contains(err.Error(), "both add and remove") {
		t.Fatalf("expected a conflict error, got %v", err)
	}
}

func TestTrashReplacesMailboxes(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 1)
	f.email("e000").MailboxIDs = map[string]bool{"mb-inbox": true, "mb-proj": true}
	m := f.client(t).Mail()

	if err := m.Trash(testCtx(t), []string{"e000"}); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	patch := f.captured("Email/set")[0]["update"].(map[string]any)["e000"].(map[string]any)
	want := map[string]any{"mailboxIds": map[string]any{"mb-trash": true}}
	if !reflect.DeepEqual(patch, want) {
		t.Errorf("patch = %#v, want %#v", patch, want)
	}
	if mb := f.email("e000").MailboxIDs; len(mb) != 1 || !mb["mb-trash"] {
		t.Errorf("mailboxIds after trash = %v", mb)
	}
}

// Restore is the ordinary additive/subtractive SetMailboxes, unlike Trash: a
// mailbox picked up along the way (mb-proj here) survives the round trip.
func TestRestoreAddsInboxAndDropsArchiveAndTrash(t *testing.T) {
	f := newFakeServer(t)
	seedEmails(f, 1)
	f.email("e000").MailboxIDs = map[string]bool{"mb-trash": true, "mb-proj": true}
	m := f.client(t).Mail()

	if err := m.Restore(testCtx(t), []string{"e000"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	mb := f.email("e000").MailboxIDs
	if !mb["mb-inbox"] {
		t.Errorf("mailboxIds after restore = %v, want mb-inbox", mb)
	}
	if mb["mb-trash"] {
		t.Errorf("mailboxIds after restore = %v, want mb-trash gone", mb)
	}
	if !mb["mb-proj"] {
		t.Errorf("mailboxIds after restore = %v, want mb-proj kept", mb)
	}
}

func TestCreateDraft(t *testing.T) {
	f := newFakeServer(t)
	m := f.client(t).Mail()
	raw := []byte("From: me@example.com\r\nSubject: hi\r\n\r\nhello\r\n")

	id, err := m.CreateDraft(testCtx(t), raw)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	e := f.email(id)
	if e == nil {
		t.Fatalf("draft %q not stored", id)
	}
	if !e.MailboxIDs["mb-drafts"] {
		t.Errorf("draft mailboxes = %v", e.MailboxIDs)
	}
	if !e.Keywords["$draft"] || !e.Keywords["$seen"] {
		t.Errorf("draft keywords = %v", e.Keywords)
	}
	if !reflect.DeepEqual(f.blobs[e.BlobID], raw) {
		t.Error("uploaded blob does not match the raw message")
	}
}

func TestSend(t *testing.T) {
	f := newFakeServer(t)
	m := f.client(t).Mail()
	raw := []byte("From: me@example.com\r\nTo: you@example.com\r\nSubject: hi\r\n\r\nhello\r\n")

	id, err := m.Send(testCtx(t), raw, "")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	e := f.email(id)
	if e == nil {
		t.Fatalf("sent message %q not stored", id)
	}
	// onSuccessUpdateEmail must have moved it to Sent and cleared $draft.
	if !e.MailboxIDs["mb-sent"] || e.MailboxIDs["mb-drafts"] {
		t.Errorf("mailboxes after send = %v", e.MailboxIDs)
	}
	if e.Keywords["$draft"] {
		t.Errorf("$draft still set after send: %v", e.Keywords)
	}

	subs := f.captured("EmailSubmission/set")
	if len(subs) != 1 {
		t.Fatalf("got %d EmailSubmission/set calls", len(subs))
	}
	create := subs[0]["create"].(map[string]any)["sub"].(map[string]any)
	if create["identityId"] != "identity-1" {
		t.Errorf("identityId = %v, want the identity matching the account email", create["identityId"])
	}
	if create["emailId"] != id {
		t.Errorf("emailId = %v, want %q", create["emailId"], id)
	}
	onSuccess := subs[0]["onSuccessUpdateEmail"].(map[string]any)
	patch, ok := onSuccess["#sub"].(map[string]any)
	if !ok {
		t.Fatalf("onSuccessUpdateEmail keyed by %v, want \"#sub\"", sortedMapKeys(onSuccess))
	}
	if patch["keywords/$draft"] != nil {
		t.Errorf("expected keywords/$draft: null, got %v", patch["keywords/$draft"])
	}
	if mb, _ := patch["mailboxIds"].(map[string]any); mb["mb-sent"] != true {
		t.Errorf("expected the patch to move the message to Sent, got %v", patch["mailboxIds"])
	}
}

// Bcc recipients are the reason Submit exists. The message deliberately carries
// no Bcc header -- it must not reach the recipients -- and RFC 8621 generates a
// null envelope's rcptTo from "the To, Cc, and Bcc header fields", so leaving the
// server to derive one silently drops every blind recipient. Submit must state
// rcptTo explicitly.
func TestSubmitStatesTheEnvelopeSoBccIsNotDropped(t *testing.T) {
	f := newFakeServer(t)
	m := f.client(t).Mail()
	// No Bcc header, exactly as mime.Build produces it.
	raw := []byte("From: me@example.com\r\nTo: you@example.com\r\nSubject: hi\r\n\r\nhello\r\n")

	env := provider.SubmitEnvelope{
		From: "me@example.com",
		To:   []string{"you@example.com", "blind@example.com"},
	}
	if _, err := m.Submit(testCtx(t), raw, env); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	subs := f.captured("EmailSubmission/set")
	if len(subs) != 1 {
		t.Fatalf("got %d EmailSubmission/set calls", len(subs))
	}
	create := subs[0]["create"].(map[string]any)["sub"].(map[string]any)
	envArg, ok := create["envelope"].(map[string]any)
	if !ok {
		t.Fatal("no envelope on the submission; the server would derive one and drop the Bcc")
	}
	if from, _ := envArg["mailFrom"].(map[string]any); from["email"] != "me@example.com" {
		t.Errorf("mailFrom = %v", envArg["mailFrom"])
	}
	rcpt, _ := envArg["rcptTo"].([]any)
	var got []string
	for _, r := range rcpt {
		if e, ok := r.(map[string]any); ok {
			got = append(got, fmt.Sprint(e["email"]))
		}
	}
	if len(got) != 2 || got[0] != "you@example.com" || got[1] != "blind@example.com" {
		t.Errorf("rcptTo = %v, want both the visible and the blind recipient", got)
	}
}

// Send without an envelope stays as it was: the server derives one. Backends
// are free to keep using it when the caller has no envelope to give.
func TestSendOmitsTheEnvelope(t *testing.T) {
	f := newFakeServer(t)
	m := f.client(t).Mail()
	raw := []byte("From: me@example.com\r\nTo: you@example.com\r\nSubject: hi\r\n\r\nhello\r\n")

	if _, err := m.Send(testCtx(t), raw, ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	create := f.captured("EmailSubmission/set")[0]["create"].(map[string]any)["sub"].(map[string]any)
	if _, present := create["envelope"]; present {
		t.Error("Send sent an envelope; it has none to send")
	}
}

// TestSendDoesNotRetrySubmission: a submission that fails with a 5xx may still
// have handed the message to the MTA, so it must be surfaced, never retried.
func TestSendDoesNotRetrySubmission(t *testing.T) {
	f := newFakeServer(t)
	f.failMethod["EmailSubmission/set"] = []int{503}
	m := f.client(t).Mail()
	raw := []byte("From: me@example.com\r\nTo: you@example.com\r\nSubject: hi\r\n\r\nhello\r\n")

	_, err := m.Send(testCtx(t), raw, "")
	if err == nil {
		t.Fatal("Send should have failed")
	}
	if n := f.attemptsFor("EmailSubmission/set"); n != 1 {
		t.Errorf("the server saw %d submissions, want exactly 1", n)
	}
	if len(f.failMethod["EmailSubmission/set"]) != 0 {
		t.Error("the scripted failure was not used")
	}
}

// TestCreateDraftDoesNotRetryImport: Email/import is not idempotent either —
// a retry would leave two copies of the draft.
func TestCreateDraftDoesNotRetryImport(t *testing.T) {
	f := newFakeServer(t)
	f.failMethod["Email/import"] = []int{503}
	m := f.client(t).Mail()

	_, err := m.CreateDraft(testCtx(t), []byte("From: me@example.com\r\n\r\nhi\r\n"))
	if err == nil {
		t.Fatal("CreateDraft should have failed")
	}
	if n := f.attemptsFor("Email/import"); n != 1 {
		t.Errorf("the server saw %d imports, want exactly 1", n)
	}
	if len(f.emails) != 0 {
		t.Errorf("a failed import left %d messages behind", len(f.emails))
	}
}

// TestIdempotentCallsStillRetry guards the other half: an ordinary read is
// still retried through the same code path.
func TestIdempotentCallsStillRetry(t *testing.T) {
	f := newFakeServer(t)
	f.failMethod["Mailbox/get"] = []int{503, 429}
	m := f.client(t).Mail()

	if _, err := m.Mailboxes(testCtx(t)); err != nil {
		t.Fatalf("Mailboxes should have succeeded after retries: %v", err)
	}
	if n := f.attemptsFor("Mailbox/get"); n != 3 {
		t.Errorf("Mailbox/get attempts = %d, want 3", n)
	}
}

// TestSubmissionAccountFromPrimaryAccounts: Identity and EmailSubmission live
// in the submission capability's primary account, not necessarily the mail one.
func TestSubmissionAccountFromPrimaryAccounts(t *testing.T) {
	f := newFakeServer(t)
	f.submissionPrimary = "acct-submission"
	m := f.client(t).Mail()

	if _, err := m.Send(testCtx(t), []byte("From: me@example.com\r\n\r\nhi\r\n"), ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ids := f.captured("Identity/get")
	if len(ids) != 1 || ids[0]["accountId"] != "acct-submission" {
		t.Errorf("Identity/get accountId = %v, want acct-submission", ids[0]["accountId"])
	}
	subs := f.captured("EmailSubmission/set")
	if len(subs) != 1 || subs[0]["accountId"] != "acct-submission" {
		t.Errorf("EmailSubmission/set accountId = %v, want acct-submission", subs[0]["accountId"])
	}
	// The message itself is still imported into the mail account.
	imports := f.captured("Email/import")
	if len(imports) != 1 || imports[0]["accountId"] != testAccount {
		t.Errorf("Email/import accountId = %v, want %s", imports[0]["accountId"], testAccount)
	}
}

// TestSubmissionAccountFallsBackToMail: a session that advertises no
// submission primary account still sends, using the mail account.
func TestSubmissionAccountFallsBackToMail(t *testing.T) {
	f := newFakeServer(t)
	f.submissionPrimary = "-" // sentinel: drop the entry entirely
	m := f.client(t).Mail()

	if _, err := m.Send(testCtx(t), []byte("From: me@example.com\r\n\r\nhi\r\n"), ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ids := f.captured("Identity/get")
	if len(ids) != 1 || ids[0]["accountId"] != testAccount {
		t.Errorf("Identity/get accountId = %v, want the mail account %s", ids[0]["accountId"], testAccount)
	}
}

func TestFetchAttachment(t *testing.T) {
	f := newFakeServer(t)
	f.mu.Lock()
	f.blobs["att-1"] = []byte("PDF-ish bytes")
	f.mu.Unlock()
	m := f.client(t).Mail()

	got, err := m.FetchAttachment(testCtx(t), "e000", "att-1")
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	if string(got) != "PDF-ish bytes" {
		t.Errorf("attachment = %q", got)
	}

	if _, err := m.FetchAttachment(testCtx(t), "e000", ""); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("empty ref error = %v, want ErrNotFound", err)
	}
	if _, err := m.FetchAttachment(testCtx(t), "e000", "nope"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("missing blob error = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Transport behaviour

func TestTransportFailureIsOffline(t *testing.T) {
	// A server that is not listening: every attempt fails at connect time.
	dead := httptest.NewServer(nil)
	url := dead.URL
	dead.Close()

	c, err := New(Options{Token: testToken, SessionURL: url + "/jmap/session"})
	if err != nil {
		t.Fatal(err)
	}
	c.retryBase = time.Millisecond

	_, err = c.Mail().Mailboxes(testCtx(t))
	if err == nil {
		t.Fatal("expected an error against a dead server")
	}
	if !provider.IsOffline(err) {
		t.Fatalf("provider.IsOffline(%v) = false, want true", err)
	}
	if !errors.Is(err, model.ErrOffline) {
		t.Fatalf("error does not wrap model.ErrOffline: %v", err)
	}
}

func TestRetriesRateLimitAndServerErrors(t *testing.T) {
	f := newFakeServer(t)
	f.failAPI = []int{429, 503, 500}
	m := f.client(t).Mail()

	if _, err := m.Mailboxes(testCtx(t)); err != nil {
		t.Fatalf("Mailboxes should have succeeded after retries: %v", err)
	}
	if len(f.failAPI) != 0 {
		t.Errorf("%d scripted failures left unused", len(f.failAPI))
	}
}

func TestGivesUpAfterRepeatedServerErrors(t *testing.T) {
	f := newFakeServer(t)
	f.failAPI = []int{500, 500, 500, 500, 500, 500}
	m := f.client(t).Mail()

	_, err := m.Mailboxes(testCtx(t))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !provider.IsOffline(err) {
		t.Fatalf("persistent 5xx should look offline, got %v", err)
	}
}

// TestForbiddenIsNotAnAuthError: 403 means "not allowed to do this", not "your
// token is dead". It must surface as an ordinary request error so a single
// forbidden call cannot be mistaken for a permanently broken credential.
func TestForbiddenIsNotAnAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"type":"about:blank","detail":"no calendars scope"}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	c, err := New(Options{Token: testToken, SessionURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.retryBase = time.Millisecond

	_, err = c.Session(testCtx(t))
	var ae *AuthError
	if errors.As(err, &ae) {
		t.Fatalf("403 gave %v, want a RequestError rather than an AuthError", err)
	}
	var re *RequestError
	if !errors.As(err, &re) || re.Status != http.StatusForbidden {
		t.Fatalf("403 gave %v, want a *RequestError with status 403", err)
	}
	if re.Detail != "no calendars scope" {
		t.Errorf("problem+json detail = %q", re.Detail)
	}
}

// TestForbiddenCalendarMethodIsNotSupported: a method-level "forbidden" for the
// calendars capability is what a Fastmail token without the Calendars scope
// looks like once the session has (misleadingly) advertised the capability. It
// must name the scope and mark the resource unsupported, so the sync engine
// skips calendars instead of failing the whole account.
func TestForbiddenCalendarMethodIsNotSupported(t *testing.T) {
	f := newFakeServer(t)
	f.refuseMethod("Calendar/get", "forbidden")
	cal := f.client(t).Calendar()

	_, err := cal.Calendars(testCtx(t))
	if err == nil {
		t.Fatal("Calendars succeeded despite a forbidden Calendar/get")
	}
	if !errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("error %v does not wrap provider.ErrNotSupported", err)
	}
	if !IsMethodError(err, "forbidden") {
		t.Errorf("error %v lost the underlying *MethodError", err)
	}
	if !strings.Contains(err.Error(), "Calendars scope") {
		t.Errorf("error %q does not name the missing token scope", err)
	}
}

// TestForbiddenMailMethodStillFails: on mail, "forbidden" is at least as likely
// to be one operation an ACL refuses as a missing scope, so it must stay a hard
// failure — with a hint, but not a licence to skip the mailbox.
func TestForbiddenMailMethodStillFails(t *testing.T) {
	f := newFakeServer(t)
	f.refuseMethod("Email/query", "forbidden")
	m := f.client(t).Mail()

	_, _, err := m.Enumerate(testCtx(t), "", 10)
	if err == nil {
		t.Fatal("Enumerate succeeded despite a forbidden Email/query")
	}
	if errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("a forbidden mail call must not be reported as an unsupported resource: %v", err)
	}
	if !IsMethodError(err, "forbidden") {
		t.Errorf("error %v is not a *MethodError", err)
	}
	if !strings.Contains(err.Error(), "Mail scope") {
		t.Errorf("error %q does not mention the scope the call needed", err)
	}
}

// TestAccountNotSupportedByMethodIsNotSupported: unlike "forbidden", this one
// is unambiguous — the account cannot serve the method at all.
func TestAccountNotSupportedByMethodIsNotSupported(t *testing.T) {
	f := newFakeServer(t)
	f.refuseMethod("Email/query", "accountNotSupportedByMethod")
	m := f.client(t).Mail()

	_, _, err := m.Enumerate(testCtx(t), "", 10)
	if !errors.Is(err, provider.ErrNotSupported) {
		t.Fatalf("error %v does not wrap provider.ErrNotSupported", err)
	}
	if !IsMethodError(err, "accountNotSupportedByMethod") {
		t.Errorf("error %v lost the underlying *MethodError", err)
	}
}

// TestAuthErrorNamesScope: a 401 on an API request knows which capabilities the
// request claimed, so `emlcal doctor` can say which box to tick.
func TestAuthErrorNamesScope(t *testing.T) {
	f := newFakeServer(t)
	f.failMethod["Email/query"] = []int{http.StatusUnauthorized}
	m := f.client(t).Mail()

	_, _, err := m.Enumerate(testCtx(t), "", 10)
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v, want an *AuthError", err)
	}
	if ae.Scopes != "the Mail scope" {
		t.Errorf("AuthError.Scopes = %q, want %q", ae.Scopes, "the Mail scope")
	}
	if !strings.Contains(err.Error(), "the Mail scope") {
		t.Errorf("error %q does not name the scope", err)
	}
}

func TestScopeHint(t *testing.T) {
	for _, tc := range []struct {
		using []string
		want  string
	}{
		{nil, ""},
		{[]string{CapCore}, ""},
		{[]string{CapCore, CapMail}, "the Mail scope"},
		{[]string{CapMail, CapSubmission}, "the Mail scope"}, // one scope covers both
		{[]string{CapCalendars}, "the Calendars scope"},
		{[]string{"https://www.fastmail.com/dev/calendars"}, "the Calendars scope"},
		{[]string{CapMail, CapCalendars}, "the Mail and Calendars scopes"},
		{[]string{"urn:example:unknown"}, ""},
	} {
		if got := scopeHint(tc.using); got != tc.want {
			t.Errorf("scopeHint(%v) = %q, want %q", tc.using, got, tc.want)
		}
	}
}

func TestBadTokenIsAuthError(t *testing.T) {
	f := newFakeServer(t)
	c, err := New(Options{Token: "wrong", SessionURL: f.srv.URL + "/jmap/session"})
	if err != nil {
		t.Fatal(err)
	}
	c.retryBase = time.Millisecond

	_, err = c.Session(testCtx(t))
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v, want *AuthError", err)
	}
	if provider.IsOffline(err) {
		t.Error("an auth failure must not be reported as offline")
	}
}

func TestMethodErrorIsTyped(t *testing.T) {
	f := newFakeServer(t)
	c := f.client(t)
	ctx := testCtx(t)

	_, err := c.Request(ctx, []string{CapMail}, []Invocation{
		{Name: "Nonsense/get", Args: map[string]any{"accountId": testAccount}, ID: "0"},
	})
	var me *MethodError
	if !errors.As(err, &me) {
		t.Fatalf("error = %v, want *MethodError", err)
	}
	if me.Type != "unknownMethod" {
		t.Errorf("type = %q", me.Type)
	}
	if me.Method != "Nonsense/get" || me.CallID != "0" {
		t.Errorf("method/call = %q/%q", me.Method, me.CallID)
	}
	if !IsMethodError(err, "unknownMethod") {
		t.Error("IsMethodError should match")
	}
	if IsMethodError(err, "somethingElse") {
		t.Error("IsMethodError matched the wrong type")
	}
}

func TestUploadAndDownloadRoundTrip(t *testing.T) {
	f := newFakeServer(t)
	c := f.client(t)
	ctx := testCtx(t)
	data := []byte("hello blob")

	blobID, size, err := c.Upload(ctx, testAccount, "message/rfc822", data)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}
	got, err := c.Download(ctx, testAccount, blobID, "x.eml", "message/rfc822")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("round trip = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Pure helpers

func TestExpandTemplateEscaping(t *testing.T) {
	tmpl := "https://x/{accountId}/{blobId}/{name}?type={type}"
	got := expandTemplate(tmpl, map[string]string{
		"accountId": "a b",
		"blobId":    "G/1&2",
		"name":      "réçu.pdf",
		"type":      "application/pdf",
	})
	want := "https://x/a%20b/G%2F1%262/r%C3%A9%C3%A7u.pdf?type=application%2Fpdf"
	if got != want {
		t.Errorf("expandTemplate =\n%q\nwant\n%q", got, want)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("3"); d != 3*time.Second {
		t.Errorf("seconds form = %v", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Errorf("empty = %v", d)
	}
	if d := parseRetryAfter("garbage"); d != 0 {
		t.Errorf("garbage = %v", d)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC1123)
	if d := parseRetryAfter(past); d != 0 {
		t.Errorf("past date = %v", d)
	}
}

func TestKeywordsToFlags(t *testing.T) {
	got := keywordsToFlags(map[string]bool{"$flagged": true, "$answered": true})
	want := model.Flags{Unread: true, Flagged: true, Answered: true}
	if got != want {
		t.Errorf("flags = %+v, want %+v", got, want)
	}
	got = keywordsToFlags(map[string]bool{"$seen": true, "$draft": true})
	want = model.Flags{Draft: true}
	if got != want {
		t.Errorf("flags = %+v, want %+v", got, want)
	}
}
