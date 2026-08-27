package gmail

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gmailapi "google.golang.org/api/gmail/v1"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newMail(t *testing.T, f *fakeGmail, tweak func(*Options)) *Mail {
	t.Helper()
	opts := f.options()
	if tweak != nil {
		tweak(&opts)
	}
	m, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestNewRequiresHTTPClient(t *testing.T) {
	if _, err := New(context.Background(), Options{}); err == nil {
		t.Fatal("New without an HTTP client succeeded, want an error")
	}
}

func TestMailboxes(t *testing.T) {
	f := newFakeGmail(t)
	f.labels = []*gmailapi.Label{
		{Id: "INBOX", Name: "INBOX", Type: "system"},
		{Id: "SENT", Name: "SENT", Type: "system"},
		{Id: "DRAFT", Name: "DRAFT", Type: "system"},
		{Id: "TRASH", Name: "TRASH", Type: "system"},
		{Id: "SPAM", Name: "SPAM", Type: "system"},
		{Id: "IMPORTANT", Name: "IMPORTANT", Type: "system"},
		{Id: "STARRED", Name: "STARRED", Type: "system"},
		{Id: "UNREAD", Name: "UNREAD", Type: "system"},
		{Id: "CHAT", Name: "CHAT", Type: "system"},
		{Id: "CATEGORY_PROMOTIONS", Name: "CATEGORY_PROMOTIONS", Type: "system"},
		{Id: "CATEGORY_PERSONAL", Name: "CATEGORY_PERSONAL", Type: "system"},
		{Id: "Label_1", Name: "clients", Type: "user"},
		{Id: "Label_2", Name: "clients/acme", Type: "user"},
		{Id: "Label_3", Name: "receipts", Type: "user"},
	}
	f.counts["INBOX"] = [2]int64{42, 7}
	f.counts["Label_2"] = [2]int64{3, 1}

	m := newMail(t, f, nil)
	boxes, err := m.Mailboxes(context.Background())
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}

	byID := map[string]model.Mailbox{}
	for _, b := range boxes {
		if _, dup := byID[b.RemoteID]; dup {
			t.Errorf("mailbox %s returned twice", b.RemoteID)
		}
		byID[b.RemoteID] = b
	}
	for _, skipped := range []string{"STARRED", "UNREAD"} {
		if _, ok := byID[skipped]; ok {
			t.Errorf("%s must not be a mailbox: it is a flag", skipped)
		}
	}
	wantRoles := map[string]model.MailboxRole{
		"INBOX":               model.RoleInbox,
		"SENT":                model.RoleSent,
		"DRAFT":               model.RoleDrafts,
		"TRASH":               model.RoleTrash,
		"SPAM":                model.RoleJunk,
		"IMPORTANT":           model.RoleImportant,
		"CATEGORY_PROMOTIONS": "category:promotions",
		"CATEGORY_PERSONAL":   "category:personal",
		"CHAT":                "",
		"Label_1":             "",
		"Label_2":             "",
	}
	for id, want := range wantRoles {
		got, ok := byID[id]
		if !ok {
			t.Errorf("mailbox %s missing", id)
			continue
		}
		if got.Role != want {
			t.Errorf("mailbox %s role = %q, want %q", id, got.Role, want)
		}
	}
	if got := byID["CHAT"].Name; got != "Chat" {
		t.Errorf("CHAT name = %q, want Chat", got)
	}
	if got := byID["INBOX"].Name; got != "Inbox" {
		t.Errorf("INBOX name = %q, want Inbox", got)
	}
	if got := byID["Label_2"]; got.ParentRemote != "Label_1" || got.Name != "acme" {
		t.Errorf("nested label = {name:%q parent:%q}, want {acme Label_1}", got.Name, got.ParentRemote)
	}
	if got := byID["Label_3"]; got.ParentRemote != "" || got.Name != "receipts" {
		t.Errorf("top-level user label = {name:%q parent:%q}, want {receipts \"\"}", got.Name, got.ParentRemote)
	}
	if got := byID["INBOX"]; got.TotalCount != 42 || got.UnreadCount != 7 {
		t.Errorf("INBOX counts = %d/%d, want 42/7", got.TotalCount, got.UnreadCount)
	}
	if got := byID["Label_2"]; got.TotalCount != 3 || got.UnreadCount != 1 {
		t.Errorf("Label_2 counts = %d/%d, want 3/1", got.TotalCount, got.UnreadCount)
	}
	// Inbox sorts before user labels.
	if boxes[0].RemoteID != "INBOX" {
		t.Errorf("first mailbox = %s, want INBOX", boxes[0].RemoteID)
	}
}

func TestMailboxesSkipsUserCountsWhenMany(t *testing.T) {
	f := newFakeGmail(t)
	f.labels = []*gmailapi.Label{{Id: "INBOX", Name: "INBOX", Type: "system"}}
	for i := range maxLabelCountFetches + 1 {
		f.labels = append(f.labels, &gmailapi.Label{
			Id: fmt.Sprintf("Label_%d", i), Name: fmt.Sprintf("l%d", i), Type: "user",
		})
	}
	m := newMail(t, f, nil)
	if _, err := m.Mailboxes(context.Background()); err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labelGets != 1 {
		t.Errorf("labels.get called %d times, want 1 (system labels only)", f.labelGets)
	}
}

func TestState(t *testing.T) {
	f := newFakeGmail(t)
	f.historyID = 987654
	m := newMail(t, f, nil)
	state, err := m.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state != "987654" {
		t.Errorf("State = %q, want 987654", state)
	}
}

func TestTotal(t *testing.T) {
	f := newFakeGmail(t)
	for i := range 7 {
		f.addMessage(fmt.Sprintf("m%d", i), "", nil, "")
	}
	m := newMail(t, f, nil)
	n, err := m.Total(context.Background())
	if err != nil {
		t.Fatalf("Total: %v", err)
	}
	if n != 7 {
		t.Errorf("Total = %d, want 7", n)
	}
}

func TestEnumeratePaging(t *testing.T) {
	f := newFakeGmail(t)
	f.pageSize = 2
	for i := 1; i <= 5; i++ {
		f.addMessage(fmt.Sprintf("m%d", i), fmt.Sprintf("t%d", i), []string{"INBOX"}, "raw")
	}
	f.addMessage("spam1", "ts", []string{"SPAM"}, "raw")

	m := newMail(t, f, func(o *Options) { o.IncludeSpamTrash = true })
	var ids []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("Enumerate did not terminate")
		}
		page, next, err := m.Enumerate(context.Background(), cursor, 0)
		if err != nil {
			t.Fatalf("Enumerate: %v", err)
		}
		for _, env := range page {
			ids = append(ids, env.RemoteID)
			if env.ThreadID == "" {
				t.Errorf("envelope %s has no thread id", env.RemoteID)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	want := []string{"m1", "m2", "m3", "m4", "m5", "spam1"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("enumerated %v, want %v", ids, want)
	}

	// Without includeSpamTrash the spam message disappears.
	m2 := newMail(t, f, nil)
	page, _, err := m2.Enumerate(context.Background(), "", 100)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	for _, env := range page {
		if env.RemoteID == "spam1" {
			t.Error("spam message listed even though IncludeSpamTrash is false")
		}
	}
}

func collectRaw(t *testing.T, m *Mail, ids []string) map[string]provider.RawMessage {
	t.Helper()
	got := map[string]provider.RawMessage{}
	var mu sync.Mutex
	err := m.FetchRaw(context.Background(), ids, func(rm provider.RawMessage) error {
		mu.Lock()
		defer mu.Unlock()
		got[rm.RemoteID] = rm
		return nil
	})
	if err != nil {
		t.Fatalf("FetchRaw: %v", err)
	}
	return got
}

func TestFetchRawBatch(t *testing.T) {
	f := newFakeGmail(t)
	var ids []string
	for i := 1; i <= 60; i++ {
		id := fmt.Sprintf("m%d", i)
		f.addMessage(id, "t1", []string{"INBOX", "UNREAD"}, "From: a@b\r\n\r\nbody "+id)
		ids = append(ids, id)
	}
	ids = append(ids, "gone") // never existed: must be skipped silently

	m := newMail(t, f, nil)
	got := collectRaw(t, m, ids)

	if len(got) != 60 {
		t.Fatalf("fetched %d messages, want 60", len(got))
	}
	if _, ok := got["gone"]; ok {
		t.Error("a 404 part was not skipped")
	}
	rm := got["m7"]
	if string(rm.Raw) != "From: a@b\r\n\r\nbody m7" {
		t.Errorf("raw body = %q", rm.Raw)
	}
	if !rm.Flags.Unread || rm.Flags.Flagged {
		t.Errorf("flags = %+v, want unread only", rm.Flags)
	}
	if !reflect.DeepEqual(rm.Mailboxes, []string{"INBOX"}) {
		t.Errorf("mailboxes = %v, want [INBOX]", rm.Mailboxes)
	}
	if rm.Received.IsZero() || rm.Received.UnixMilli() != 1700000000000 {
		t.Errorf("received = %v, want the internalDate", rm.Received)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// 61 ids over batches of 50 = 2 HTTP requests.
	if f.batchCalls != 2 {
		t.Errorf("batch endpoint hit %d times, want 2", f.batchCalls)
	}
}

func TestFetchRawRetriesThrottledPart(t *testing.T) {
	f := newFakeGmail(t)
	for _, id := range []string{"a", "b", "c"} {
		f.addMessage(id, "t", []string{"INBOX"}, "raw "+id)
	}
	f.failOnce["b"] = http.StatusTooManyRequests

	m := newMail(t, f, nil)
	got := collectRaw(t, m, []string{"a", "b", "c"})
	if len(got) != 3 {
		t.Fatalf("fetched %d messages, want 3 (the 429 part must be retried)", len(got))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.batchCalls != 2 {
		t.Errorf("batch calls = %d, want 2 (initial + retry of the throttled id)", f.batchCalls)
	}
}

func TestFetchRawIndividualFallback(t *testing.T) {
	f := newFakeGmail(t)
	for i := 1; i <= 5; i++ {
		f.addMessage(fmt.Sprintf("m%d", i), "t", []string{"INBOX", "STARRED", "DRAFT"}, "body")
	}
	m := newMail(t, f, func(o *Options) { o.FetchMode = FetchIndividual })
	got := collectRaw(t, m, []string{"m1", "m2", "m3", "m4", "m5", "nope"})
	if len(got) != 5 {
		t.Fatalf("fetched %d messages, want 5", len(got))
	}
	rm := got["m1"]
	if !rm.Flags.Flagged || !rm.Flags.Draft {
		t.Errorf("flags = %+v, want flagged+draft", rm.Flags)
	}
	// DRAFT is a real mailbox as well as a flag; STARRED is not.
	if !reflect.DeepEqual(rm.Mailboxes, []string{"INBOX", "DRAFT"}) {
		t.Errorf("mailboxes = %v, want [INBOX DRAFT]", rm.Mailboxes)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.batchCalls != 0 {
		t.Errorf("batch endpoint used %d times in individual mode", f.batchCalls)
	}
}

func TestFetchRawPropagatesCallbackError(t *testing.T) {
	f := newFakeGmail(t)
	f.addMessage("m1", "t", []string{"INBOX"}, "body")
	m := newMail(t, f, nil)
	sentinel := errors.New("stop")
	err := m.FetchRaw(context.Background(), []string{"m1"}, func(provider.RawMessage) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("FetchRaw error = %v, want the callback's error", err)
	}
}

func TestRateLimiterBudget(t *testing.T) {
	f := newFakeGmail(t)
	var ids []string
	for i := range 100 {
		id := fmt.Sprintf("m%d", i)
		f.addMessage(id, "t", []string{"INBOX"}, "b")
		ids = append(ids, id)
	}
	// 100 messages × 20 units = 2000 units. With a 1000-unit burst and 2500
	// units/s of refill the fetch cannot finish faster than 400ms.
	m := newMail(t, f, func(o *Options) { o.QuotaUnitsPerSecond = 2500 })
	start := time.Now()
	got := collectRaw(t, m, ids)
	elapsed := time.Since(start)
	if len(got) != 100 {
		t.Fatalf("fetched %d, want 100", len(got))
	}
	if elapsed < 80*time.Millisecond {
		t.Errorf("fetch took %v; the quota limiter did not throttle the burst", elapsed)
	}
}

func TestChangesCoalescing(t *testing.T) {
	f := newFakeGmail(t)
	f.historyID = 2000
	// m-live exists and had a label added; m-new is brand new; m-gone was
	// added and then deleted inside the same window.
	f.addMessage("m-live", "t1", []string{"INBOX", "IMPORTANT"}, "raw")
	f.addMessage("m-new", "t2", []string{"INBOX"}, "raw")
	f.history = []*gmailapi.History{
		{Id: 1001, MessagesAdded: []*gmailapi.HistoryMessageAdded{
			{Message: &gmailapi.Message{Id: "m-new", ThreadId: "t2"}},
			{Message: &gmailapi.Message{Id: "m-gone", ThreadId: "t3"}},
		}},
		{Id: 1002, LabelsAdded: []*gmailapi.HistoryLabelAdded{
			{Message: &gmailapi.Message{Id: "m-live", ThreadId: "t1"}, LabelIds: []string{"IMPORTANT"}},
		}},
		{Id: 1003, LabelsRemoved: []*gmailapi.HistoryLabelRemoved{
			{Message: &gmailapi.Message{Id: "m-live", ThreadId: "t1"}, LabelIds: []string{"UNREAD"}},
		}},
		{Id: 1004, MessagesDeleted: []*gmailapi.HistoryMessageDeleted{
			{Message: &gmailapi.Message{Id: "m-gone", ThreadId: "t3"}},
		}},
	}

	m := newMail(t, f, nil)
	ch, err := m.Changes(context.Background(), "1000")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(ch.Added) != 1 || ch.Added[0].RemoteID != "m-new" || ch.Added[0].ThreadID != "t2" {
		t.Errorf("Added = %+v, want just m-new", ch.Added)
	}
	if !reflect.DeepEqual(ch.Removed, []string{"m-gone"}) {
		t.Errorf("Removed = %v, want [m-gone] (add+delete collapses to delete)", ch.Removed)
	}
	if len(ch.Updated) != 1 {
		t.Fatalf("Updated = %+v, want one entry", ch.Updated)
	}
	up := ch.Updated[0]
	if up.RemoteID != "m-live" {
		t.Errorf("Updated id = %q, want m-live", up.RemoteID)
	}
	if !reflect.DeepEqual(up.Mailboxes, []string{"INBOX", "IMPORTANT"}) {
		t.Errorf("Updated mailboxes = %v, want the current server labels", up.Mailboxes)
	}
	if up.Flags.Unread {
		t.Error("Updated flags still say unread; current labels were not re-read")
	}
	if ch.NewState != "2000" {
		t.Errorf("NewState = %q, want 2000 (the mailbox history id)", ch.NewState)
	}
}

func TestChangesUpdatedMessageVanished(t *testing.T) {
	f := newFakeGmail(t)
	f.historyID = 2000
	f.history = []*gmailapi.History{
		{Id: 1002, LabelsAdded: []*gmailapi.HistoryLabelAdded{
			{Message: &gmailapi.Message{Id: "ghost", ThreadId: "t"}, LabelIds: []string{"INBOX"}},
		}},
	}
	m := newMail(t, f, nil)
	ch, err := m.Changes(context.Background(), "1000")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(ch.Updated) != 0 {
		t.Errorf("Updated = %+v, want none", ch.Updated)
	}
	if !reflect.DeepEqual(ch.Removed, []string{"ghost"}) {
		t.Errorf("Removed = %v, want [ghost]", ch.Removed)
	}
}

func TestChangesEmptyWindow(t *testing.T) {
	f := newFakeGmail(t)
	f.historyID = 1000
	m := newMail(t, f, nil)
	ch, err := m.Changes(context.Background(), "1000")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(ch.Added)+len(ch.Updated)+len(ch.Removed) != 0 {
		t.Errorf("empty history produced changes: %+v", ch)
	}
	if ch.NewState != "1000" {
		t.Errorf("NewState = %q, want the profile history id 1000", ch.NewState)
	}
}

func TestChangesMailboxesChanged(t *testing.T) {
	f := newFakeGmail(t)
	f.labels = []*gmailapi.Label{{Id: "INBOX", Name: "INBOX", Type: "system"}}
	f.historyID = 2000
	f.addMessage("m1", "t", []string{"INBOX"}, "raw")
	f.history = []*gmailapi.History{
		{Id: 1002, LabelsAdded: []*gmailapi.HistoryLabelAdded{
			{Message: &gmailapi.Message{Id: "m1"}, LabelIds: []string{"Label_new"}},
		}},
	}
	m := newMail(t, f, nil)
	if _, err := m.Mailboxes(context.Background()); err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	ch, err := m.Changes(context.Background(), "1000")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if !ch.MailboxesChanged {
		t.Error("MailboxesChanged = false, want true after an unknown label appeared")
	}
}

func TestChangesStateExpired(t *testing.T) {
	f := newFakeGmail(t)
	f.historyGone = true
	m := newMail(t, f, nil)
	if _, err := m.Changes(context.Background(), "1"); !errors.Is(err, provider.ErrStateExpired) {
		t.Fatalf("Changes with a stale history id = %v, want provider.ErrStateExpired", err)
	}
	if _, err := m.Changes(context.Background(), ""); !errors.Is(err, provider.ErrStateExpired) {
		t.Fatalf("Changes with an empty state = %v, want provider.ErrStateExpired", err)
	}
	if _, err := m.Changes(context.Background(), "not-a-number"); !errors.Is(err, provider.ErrStateExpired) {
		t.Fatalf("Changes with a junk state = %v, want provider.ErrStateExpired", err)
	}
}

func TestWrites(t *testing.T) {
	f := newFakeGmail(t)
	f.addMessage("m1", "t1", []string{"INBOX", "UNREAD"}, "raw")
	f.attachments["m1/att-1"] = base64.RawURLEncoding.EncodeToString([]byte("PDF-BYTES"))
	m := newMail(t, f, nil)
	ctx := context.Background()

	if err := m.SetFlags(ctx, []string{"m1"}, model.Flags{Flagged: true}, model.Flags{Unread: true}); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	f.mu.Lock()
	mod := f.lastModify
	f.mu.Unlock()
	if !reflect.DeepEqual(mod.AddLabelIds, []string{"STARRED"}) ||
		!reflect.DeepEqual(mod.RemoveLabelIds, []string{"UNREAD"}) ||
		!reflect.DeepEqual(mod.Ids, []string{"m1"}) {
		t.Errorf("batchModify request = %+v, want +STARRED -UNREAD on m1", mod)
	}

	if err := m.SetMailboxes(ctx, []string{"m1"}, []string{"Label_1"}, []string{"INBOX"}); err != nil {
		t.Fatalf("SetMailboxes: %v", err)
	}
	f.mu.Lock()
	mod = f.lastModify
	f.mu.Unlock()
	if !reflect.DeepEqual(mod.AddLabelIds, []string{"Label_1"}) ||
		!reflect.DeepEqual(mod.RemoveLabelIds, []string{"INBOX"}) {
		t.Errorf("batchModify request = %+v, want +Label_1 -INBOX", mod)
	}

	// A no-op change must not reach the API at all.
	f.mu.Lock()
	f.lastModify = nil
	f.mu.Unlock()
	if err := m.SetFlags(ctx, []string{"m1"}, model.Flags{}, model.Flags{}); err != nil {
		t.Fatalf("SetFlags (no-op): %v", err)
	}
	f.mu.Lock()
	if f.lastModify != nil {
		t.Error("an empty flag change still called batchModify")
	}
	f.mu.Unlock()

	if err := m.Trash(ctx, []string{"m1", "already-gone"}); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	f.mu.Lock()
	if !reflect.DeepEqual(f.trashed, []string{"m1"}) {
		t.Errorf("trashed = %v, want [m1] (a missing id is not an error)", f.trashed)
	}
	f.mu.Unlock()

	id, err := m.CreateDraft(ctx, []byte("Subject: hi\r\n\r\nbody"))
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if id != "draft-msg-1" {
		t.Errorf("CreateDraft id = %q, want the draft's message id", id)
	}
	f.mu.Lock()
	gotRaw, _ := base64.RawURLEncoding.DecodeString(strings.TrimRight(f.draftedRaw, "="))
	f.mu.Unlock()
	if string(gotRaw) != "Subject: hi\r\n\r\nbody" {
		t.Errorf("draft raw = %q", gotRaw)
	}

	sentID, err := m.Send(ctx, []byte("Subject: re\r\n\r\nbody"), "t1")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sentID != "sent-1" {
		t.Errorf("Send id = %q, want sent-1", sentID)
	}
	f.mu.Lock()
	if f.sentThread != "t1" {
		t.Errorf("sent threadId = %q, want t1", f.sentThread)
	}
	f.mu.Unlock()

	att, err := m.FetchAttachment(ctx, "m1", "att-1")
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	if string(att) != "PDF-BYTES" {
		t.Errorf("attachment = %q, want PDF-BYTES", att)
	}
	if _, err := m.FetchAttachment(ctx, "m1", "nope"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("missing attachment = %v, want model.ErrNotFound", err)
	}
}

func TestOfflineWrapping(t *testing.T) {
	f := newFakeGmail(t)
	m := newMail(t, f, nil)
	f.srv.Close() // nothing is listening any more

	if _, err := m.State(context.Background()); !errors.Is(err, model.ErrOffline) {
		t.Errorf("State while offline = %v, want model.ErrOffline", err)
	}
	if _, _, err := m.Enumerate(context.Background(), "", 0); !errors.Is(err, model.ErrOffline) {
		t.Errorf("Enumerate while offline = %v, want model.ErrOffline", err)
	}
	err := m.FetchRaw(context.Background(), []string{"m1"}, func(provider.RawMessage) error { return nil })
	if !errors.Is(err, model.ErrOffline) {
		t.Errorf("FetchRaw while offline = %v, want model.ErrOffline", err)
	}
	if !provider.IsOffline(err) {
		t.Error("provider.IsOffline does not recognise the wrapped error")
	}
}

func TestContextCancellation(t *testing.T) {
	f := newFakeGmail(t)
	f.addMessage("m1", "t", []string{"INBOX"}, "raw")
	m := newMail(t, f, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.State(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("State with a cancelled ctx = %v, want context.Canceled", err)
	}
	err := m.FetchRaw(ctx, []string{"m1"}, func(provider.RawMessage) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("FetchRaw with a cancelled ctx = %v, want context.Canceled", err)
	}
	if _, err := m.Changes(ctx, "1"); !errors.Is(err, context.Canceled) {
		t.Errorf("Changes with a cancelled ctx = %v, want context.Canceled", err)
	}
}

func TestLabelsToFlags(t *testing.T) {
	flags, boxes := labelsToFlags([]string{"UNREAD", "STARRED", "DRAFT", "INBOX", "Label_1"})
	want := model.Flags{Unread: true, Flagged: true, Draft: true}
	if flags != want {
		t.Errorf("flags = %+v, want %+v", flags, want)
	}
	sort.Strings(boxes)
	if !reflect.DeepEqual(boxes, []string{"DRAFT", "INBOX", "Label_1"}) {
		t.Errorf("mailboxes = %v; STARRED/UNREAD must not be mailboxes, DRAFT must", boxes)
	}
}

func TestDecodeBase64URL(t *testing.T) {
	for _, in := range []string{
		base64.RawURLEncoding.EncodeToString([]byte("hello?>")),
		base64.URLEncoding.EncodeToString([]byte("hello?>")),
	} {
		got, err := decodeBase64URL(in)
		if err != nil {
			t.Fatalf("decodeBase64URL(%q): %v", in, err)
		}
		if string(got) != "hello?>" {
			t.Errorf("decodeBase64URL(%q) = %q", in, got)
		}
	}
	if _, err := decodeBase64URL("!!!not base64!!!"); err == nil {
		t.Error("decodeBase64URL accepted junk")
	}
}

func TestFetchEnvelopes(t *testing.T) {
	f := newFakeGmail(t)
	f.addMessage("m1", "t1", []string{"INBOX", "UNREAD"}, "raw bytes")
	m := newMail(t, f, nil)

	var got []provider.Envelope
	err := m.FetchEnvelopes(context.Background(), []string{"m1", "missing"}, func(env provider.Envelope) error {
		got = append(got, env)
		return nil
	})
	if err != nil {
		t.Fatalf("FetchEnvelopes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d envelopes, want 1", len(got))
	}
	if !got[0].Flags.Unread || !reflect.DeepEqual(got[0].Mailboxes, []string{"INBOX"}) {
		t.Errorf("envelope = %+v, want unread + [INBOX]", got[0])
	}
	if got[0].ThreadID != "t1" {
		t.Errorf("ThreadID = %q, want t1", got[0].ThreadID)
	}
}

// A 5xx on messages.send does not mean the message was not sent: Gmail may
// have accepted it and lost the response. Retrying would deliver it twice, so
// the call must fail after exactly one attempt.
func TestSendIsNotRetriedOnServerError(t *testing.T) {
	f := newFakeGmail(t)
	f.failSendOnce = http.StatusServiceUnavailable
	m := newMail(t, f, nil)

	id, err := m.Send(context.Background(), []byte("From: a@b\r\n\r\nhi"), "")
	if err == nil {
		t.Fatalf("Send returned id %q and no error, want the 503 surfaced", id)
	}
	if id != "" {
		t.Errorf("Send returned id %q on failure, want empty", id)
	}
	if errors.Is(err, model.ErrOffline) {
		t.Errorf("Send error = %v, want a server error, not ErrOffline", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendCalls != 1 {
		t.Errorf("the fake received %d messages.send calls, want exactly 1: "+
			"a retried send duplicates the message", f.sendCalls)
	}
}

// The same holds for drafts.create: a retry leaves two drafts behind.
func TestCreateDraftIsNotRetriedOnServerError(t *testing.T) {
	f := newFakeGmail(t)
	f.failDraftOnce = http.StatusTooManyRequests
	m := newMail(t, f, nil)

	if _, err := m.CreateDraft(context.Background(), []byte("From: a@b\r\n\r\nhi")); err == nil {
		t.Fatal("CreateDraft succeeded, want the 429 surfaced")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.draftCalls != 1 {
		t.Errorf("the fake received %d drafts.create calls, want exactly 1", f.draftCalls)
	}
}

// Idempotent calls must still be retried; the guard is limited to writes.
func TestIdempotentCallsStillRetry(t *testing.T) {
	f := newFakeGmail(t)
	f.addMessage("m1", "t1", []string{"INBOX"}, "body")
	f.failOnce["m1"] = http.StatusServiceUnavailable
	m := newMail(t, f, func(o *Options) { o.FetchMode = FetchIndividual })

	got := collectRaw(t, m, []string{"m1"})
	if len(got) != 1 {
		t.Fatalf("fetched %d messages, want 1 after a retried 503", len(got))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gets != 2 {
		t.Errorf("messages.get called %d times, want 2 (one failure, one retry)", f.gets)
	}
}

// A transport failure on a non-idempotent write is still reported as offline,
// so the sync engine backs off wholesale instead of treating it as a hard
// rejection.
func TestSendTransportFailureIsOffline(t *testing.T) {
	f := newFakeGmail(t)
	m := newMail(t, f, nil)
	f.srv.Close() // nothing is listening any more

	_, err := m.Send(context.Background(), []byte("From: a@b\r\n\r\nhi"), "")
	if !errors.Is(err, model.ErrOffline) {
		t.Fatalf("Send with a dead server = %v, want model.ErrOffline", err)
	}
}

// The batch endpoint must default to the per-API host; the global
// www.googleapis.com batch endpoint is deprecated.
func TestBatchEndpointDefaults(t *testing.T) {
	f := newFakeGmail(t)

	m := newMail(t, f, func(o *Options) { o.Endpoint = "" })
	if m.batchURL != "https://gmail.googleapis.com/batch/gmail/v1" {
		t.Errorf("default batchURL = %q", m.batchURL)
	}

	m = newMail(t, f, nil) // Endpoint points at the fake
	if want := f.srv.URL + "/batch/gmail/v1"; m.batchURL != want {
		t.Errorf("batchURL with an Endpoint override = %q, want %q", m.batchURL, want)
	}

	m = newMail(t, f, func(o *Options) { o.BatchEndpoint = "https://example.test/b" })
	if m.batchURL != "https://example.test/b" {
		t.Errorf("BatchEndpoint override ignored: %q", m.batchURL)
	}
}

// Bcc is the whole reason Submit exists. messages.send reads the recipients out
// of the message headers -- there is no envelope to state -- so a message built
// without a Bcc header (which is how it must go out) reaches only the visible
// recipients. Submit reunites the two; Gmail strips the header on delivery.
func TestSubmitAddsBccForRecipientsTheMessageDoesNotName(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		to   []string
		want string // the Bcc header expected, "" for none
	}{
		{
			name: "blind recipient gains a header",
			raw:  "From: me@example.com\r\nTo: you@example.com\r\nSubject: hi\r\n\r\nbody\r\n",
			to:   []string{"you@example.com", "blind@example.com"},
			want: "Bcc: blind@example.com\r\n",
		},
		{
			name: "nothing blind, nothing added",
			raw:  "From: me@example.com\r\nTo: you@example.com\r\nSubject: hi\r\n\r\nbody\r\n",
			to:   []string{"you@example.com"},
			want: "",
		},
		{
			name: "cc counts as visible",
			raw:  "From: me@example.com\r\nTo: a@example.com\r\nCc: b@example.com\r\nSubject: hi\r\n\r\nbody\r\n",
			to:   []string{"a@example.com", "b@example.com"},
			want: "",
		},
		{
			name: "display names and case do not hide a match",
			raw:  "From: me@example.com\r\nTo: You <YOU@Example.com>\r\nSubject: hi\r\n\r\nbody\r\n",
			to:   []string{"you@example.com"},
			want: "",
		},
		{
			name: "folded header is still read",
			raw:  "From: me@example.com\r\nTo: a@example.com,\r\n b@example.com\r\nSubject: hi\r\n\r\nbody\r\n",
			to:   []string{"a@example.com", "b@example.com", "c@example.com"},
			want: "Bcc: c@example.com\r\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withBcc([]byte(tc.raw), provider.SubmitEnvelope{From: "me@example.com", To: tc.to})
			want := tc.want + tc.raw
			if string(got) != want {
				t.Errorf("withBcc =\n%q\nwant\n%q", got, want)
			}
		})
	}
}

// A body that happens to contain header-shaped lines must not be mistaken for
// recipients -- that would drop a real blind recipient.
func TestVisibleRecipientsStopsAtTheHeaderBlock(t *testing.T) {
	raw := "From: me@example.com\r\nTo: a@example.com\r\n\r\nTo: notaheader@example.com\r\n"
	got := visibleRecipients([]byte(raw))
	if got["notaheader@example.com"] {
		t.Error("read a To: line out of the body")
	}
	if !got["a@example.com"] {
		t.Error("missed the real To: header")
	}
}
