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
	for i := 1; i < len(all); i++ {
		if all[i].Received.Before(all[i-1].Received) {
			t.Fatalf("envelopes out of order at %d", i)
		}
	}
	first := all[0]
	if first.RemoteID != "e000" || first.ThreadID != "t000" {
		t.Errorf("first envelope = %+v", first)
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

// TestEnumerateSurvivesDeleteBehindCursor deletes a message that has already
// been enumerated. A position cursor would shift down by one and skip an
// unenumerated message; the anchor cursor must not.
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
		// Delete a message from the page we have just consumed, plus one from
		// the very first page, so the whole list shifts under the cursor.
		if pages == 1 {
			f.deleteEmail("e000")
			f.deleteEmail("e003")
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
		if id == "e000" || id == "e003" {
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
	if cur.Anchor != "e009" || cur.N != 10 {
		t.Fatalf("cursor = %+v, want anchor e009 and n 10", cur)
	}

	// The anchor is destroyed before the next page is requested.
	f.deleteEmail("e009")
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
	if len(page2) != 10 || page2[0].RemoteID != "e010" {
		t.Fatalf("second page = %d envelopes starting at %q, want e010 (nothing skipped)",
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
	for _, bad := range []string{"not-a-number", "-3", `{"n":-1}`, "{oops"} {
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
