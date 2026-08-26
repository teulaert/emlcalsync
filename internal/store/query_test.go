package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
)

// seedCorpus builds a small two-account corpus used by the filter, thread and
// search tests.
//
//	work:w1  base+0h   alice -> bob      "Invoice 2026-08"      inbox, unread
//	work:w2  base+1h   newsletter        "Weekly digest"        inbox, bulk
//	work:w3  base+2h   bob -> alice      "Re: Invoice 2026-08"  archive, flagged  (thread of w1)
//	work:w4  base-48h  me -> dave        "Old sent mail"        sent
//	personal:p1 base+3h carol            "Dinner Friday"        inbox, unread
func seedCorpus(t *testing.T, s *Store) {
	t.Helper()
	seedAccount(t, s, "work")
	seedAccount(t, s, "personal")

	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "w1", ThreadID: "th-invoice", Received: base,
		Flags: model.Flags{Unread: true}, MailboxRemotes: []string{"mb-inbox"},
	}, &mime.Parsed{
		Subject: "Invoice 2026-08", From: addr("Alice Example", "alice@example.com"),
		To:       []model.Address{addr("Bob Builder", "bob@example.com")},
		Date:     base,
		TextBody: "Invoice attached for August. Total 1200 EUR. Please pay the invoice before September.",
		Attachments: []mime.Part{
			{Path: "2", Filename: "invoice-august.pdf", ContentType: "application/pdf", Size: 2048},
		},
	})

	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "w2", ThreadID: "th-news", Received: base.Add(time.Hour),
		MailboxRemotes: []string{"mb-inbox"},
	}, &mime.Parsed{
		Subject: "Weekly digest", From: addr("Weekly News", "news@lists.example.com"),
		To:       []model.Address{addr("", "subscriber@example.com")},
		Date:     base.Add(time.Hour),
		TextBody: "This week in widgets: nothing happened.",
		ListID:   "widgets.lists.example.com", IsBulk: true,
	})

	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "w3", ThreadID: "th-invoice", Received: base.Add(2 * time.Hour),
		Flags: model.Flags{Flagged: true}, MailboxRemotes: []string{"mb-archive"},
	}, &mime.Parsed{
		Subject: "Re: Invoice 2026-08", From: addr("Bob Builder", "bob@example.com"),
		To:       []model.Address{addr("Alice Example", "alice@example.com")},
		Date:     base.Add(2 * time.Hour),
		TextBody: "Thanks, paid.",
	})

	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "w4", ThreadID: "th-old", Received: base.Add(-48 * time.Hour),
		MailboxRemotes: []string{"mb-sent"},
	}, &mime.Parsed{
		Subject: "Old sent mail", From: addr("Me", "work@example.com"),
		To:       []model.Address{addr("Dave", "dave@example.com")},
		Date:     base.Add(-48 * time.Hour),
		TextBody: "An older message about widgets.",
	})

	putMessage(t, s, &model.Message{
		AccountID: "personal", RemoteID: "p1", ThreadID: "th-dinner", Received: base.Add(3 * time.Hour),
		Flags: model.Flags{Unread: true}, MailboxRemotes: []string{"mb-inbox"},
	}, &mime.Parsed{
		Subject: "Dinner Friday", From: addr("Carol", "carol@other.example"),
		To:       []model.Address{addr("", "me@personal.example")},
		Date:     base.Add(3 * time.Hour),
		TextBody: "Shall we get dinner on Friday?",
	})
}

func remoteIDs(msgs []model.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.RemoteID
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListMessagesFilters(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedCorpus(t, s)

	cases := []struct {
		name   string
		filter MessageFilter
		want   []string // ordered, newest first
	}{
		{"all", MessageFilter{}, []string{"p1", "w3", "w2", "w1", "w4"}},
		{"account", MessageFilter{Accounts: []string{"work"}}, []string{"w3", "w2", "w1", "w4"}},
		{"two accounts", MessageFilter{Accounts: []string{"work", "personal"}}, []string{"p1", "w3", "w2", "w1", "w4"}},
		{"mailbox role inbox", MessageFilter{MailboxRole: "inbox"}, []string{"p1", "w2", "w1"}},
		{"mailbox role uppercase", MessageFilter{MailboxRole: "INBOX"}, []string{"p1", "w2", "w1"}},
		{"mailbox role sent", MessageFilter{MailboxRole: "sent"}, []string{"w4"}},
		{"mailbox name", MessageFilter{MailboxName: "archive"}, []string{"w3"}},
		{"unread", MessageFilter{Unread: ptrBool(true)}, []string{"p1", "w1"}},
		{"read", MessageFilter{Unread: ptrBool(false)}, []string{"w3", "w2", "w4"}},
		{"flagged", MessageFilter{Flagged: ptrBool(true)}, []string{"w3"}},
		{"from address", MessageFilter{From: "alice@"}, []string{"w1"}},
		{"from name", MessageFilter{From: "bob builder"}, []string{"w3"}},
		{"from partial", MessageFilter{From: "LISTS.EXAMPLE"}, []string{"w2"}},
		{"to", MessageFilter{To: "bob@example.com"}, []string{"w1"}},
		{"to matches cc too", MessageFilter{To: "alice"}, []string{"w3"}},
		{"since", MessageFilter{Since: base.Add(90 * time.Minute)}, []string{"p1", "w3"}},
		{"until", MessageFilter{Until: base.Add(time.Minute)}, []string{"w1", "w4"}},
		{"window", MessageFilter{Since: base, Until: base.Add(2 * time.Hour)}, []string{"w2", "w1"}},
		{"no bulk", MessageFilter{NoBulk: true}, []string{"p1", "w3", "w1", "w4"}},
		{"thread", MessageFilter{ThreadID: "th-invoice"}, []string{"w3", "w1"}},
		{"combined", MessageFilter{Accounts: []string{"work"}, MailboxRole: "inbox", NoBulk: true}, []string{"w1"}},
		{"limit", MessageFilter{Limit: 2}, []string{"p1", "w3"}},
		{"limit+offset", MessageFilter{Limit: 2, Offset: 2}, []string{"w2", "w1"}},
		{"offset only", MessageFilter{Offset: 3}, []string{"w1", "w4"}},
		{"no match", MessageFilter{From: "nobody@nowhere"}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListMessages(ctx, tc.filter)
			if err != nil {
				t.Fatalf("ListMessages: %v", err)
			}
			if !equalStrings(remoteIDs(got), tc.want) {
				t.Fatalf("got %v, want %v", remoteIDs(got), tc.want)
			}
			// CountMessages agrees with the unpaged result set.
			f := tc.filter
			f.Limit, f.Offset = 0, 0
			total, err := s.CountMessages(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			unpaged, _ := s.ListMessages(ctx, f)
			if total != len(unpaged) {
				t.Fatalf("CountMessages = %d, list returned %d", total, len(unpaged))
			}
		})
	}
}

func TestListMessagesIsCheap(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedCorpus(t, s)

	msgs, err := s.ListMessages(ctx, MessageFilter{Accounts: []string{"work"}, ThreadID: "th-invoice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.TextBody != "" {
			t.Errorf("%s: list results must not carry text_body (got %d bytes)", m.RemoteID, len(m.TextBody))
		}
		if m.Snippet == "" {
			t.Errorf("%s: snippet missing from list output", m.RemoteID)
		}
		if len(m.MailboxRemotes) == 0 {
			t.Errorf("%s: mailbox membership missing from list output", m.RemoteID)
		}
	}
	// …but the single-message getter does load it.
	full, err := s.GetMessage(ctx, "work", "w1")
	if err != nil {
		t.Fatal(err)
	}
	if full.TextBody == "" {
		t.Fatal("GetMessage did not load text_body")
	}
}

func TestThreads(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedCorpus(t, s)

	th, msgs, err := s.GetThread(ctx, "work", "th-invoice", false)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.MessageCount != 2 || th.UnreadCount != 1 {
		t.Errorf("thread counts = %d/%d, want 2/1", th.MessageCount, th.UnreadCount)
	}
	if th.Subject != "Invoice 2026-08" {
		t.Errorf("thread subject = %q, want the first message's", th.Subject)
	}
	if !th.First.Equal(base) || !th.Last.Equal(base.Add(2*time.Hour)) {
		t.Errorf("thread window = %v..%v", th.First, th.Last)
	}
	if th.PublicID() != "work:t:th-invoice" {
		t.Errorf("PublicID = %s", th.PublicID())
	}
	// Participants: alice and bob, each once, in first-seen order.
	if len(th.Participants) != 2 {
		t.Fatalf("participants = %+v", th.Participants)
	}
	if th.Participants[0].Email != "alice@example.com" || th.Participants[1].Email != "bob@example.com" {
		t.Errorf("participants = %+v", th.Participants)
	}
	if th.Participants[0].Name != "Alice Example" {
		t.Errorf("participant names lost: %+v", th.Participants[0])
	}

	// Messages come back oldest first, with bodies.
	if !equalStrings(remoteIDs(msgs), []string{"w1", "w3"}) {
		t.Fatalf("thread messages = %v, want [w1 w3]", remoteIDs(msgs))
	}
	if msgs[0].TextBody == "" {
		t.Error("thread messages must carry bodies")
	}
	if len(msgs[0].MailboxRemotes) == 0 {
		t.Error("thread messages must carry mailbox membership")
	}

	if _, _, err := s.GetThread(ctx, "work", "nope", false); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetThread(missing) = %v, want ErrNotFound", err)
	}

	// ListThreads applies the same filters, newest activity first.
	all, err := s.ListThreads(ctx, MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, x := range all {
		ids = append(ids, x.ThreadID)
	}
	if !equalStrings(ids, []string{"th-dinner", "th-invoice", "th-news", "th-old"}) {
		t.Fatalf("ListThreads = %v", ids)
	}

	work, err := s.ListThreads(ctx, MessageFilter{Accounts: []string{"work"}, MailboxRole: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	ids = nil
	for _, x := range work {
		ids = append(ids, x.ThreadID)
	}
	if !equalStrings(ids, []string{"th-invoice", "th-news"}) {
		t.Fatalf("filtered ListThreads = %v", ids)
	}

	// A thread whose only matching message is filtered out disappears.
	flagged, err := s.ListThreads(ctx, MessageFilter{Flagged: ptrBool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if len(flagged) != 1 || flagged[0].ThreadID != "th-invoice" {
		t.Fatalf("flagged threads = %+v", flagged)
	}

	// RebuildThreads reproduces the same summaries.
	before, _ := s.ListThreads(ctx, MessageFilter{Accounts: []string{"work"}})
	if err := s.RebuildThreads(ctx, "work"); err != nil {
		t.Fatalf("RebuildThreads: %v", err)
	}
	after, _ := s.ListThreads(ctx, MessageFilter{Accounts: []string{"work"}})
	if len(before) != len(after) {
		t.Fatalf("RebuildThreads changed thread count: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].ThreadID != after[i].ThreadID ||
			before[i].MessageCount != after[i].MessageCount ||
			before[i].UnreadCount != after[i].UnreadCount ||
			!before[i].Last.Equal(after[i].Last) {
			t.Fatalf("thread %d differs after rebuild:\n%+v\n%+v", i, before[i], after[i])
		}
	}
}

func TestThreadSummaryTracksMessages(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	// One message: the thread appears.
	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "a", ThreadID: "th", Received: base,
		Flags: model.Flags{Unread: true}, MailboxRemotes: []string{"mb-inbox"},
	}, &mime.Parsed{Subject: "Start", From: addr("A", "a@x.example"), TextBody: "one"})

	th, _, err := s.GetThread(ctx, "work", "th", false)
	if err != nil {
		t.Fatal(err)
	}
	if th.MessageCount != 1 || th.UnreadCount != 1 || th.Subject != "Start" {
		t.Fatalf("initial thread = %+v", th)
	}

	// A reply extends the window and the participant list.
	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "b", ThreadID: "th", Received: base.Add(time.Hour),
		MailboxRemotes: []string{"mb-inbox"},
	}, &mime.Parsed{Subject: "Re: Start", From: addr("B", "b@x.example"), TextBody: "two"})

	th, _, _ = s.GetThread(ctx, "work", "th", false)
	if th.MessageCount != 2 || th.UnreadCount != 1 {
		t.Fatalf("after reply = %+v", th)
	}
	if !th.Last.Equal(base.Add(time.Hour)) || !th.First.Equal(base) {
		t.Fatalf("window = %v..%v", th.First, th.Last)
	}
	if len(th.Participants) != 2 {
		t.Fatalf("participants = %+v", th.Participants)
	}
	if th.Subject != "Start" {
		t.Fatalf("subject should stay the thread opener's, got %q", th.Subject)
	}

	// Marking the first read updates the unread count.
	if err := s.UpdateMessageState(ctx, "work", "a", model.Flags{}, nil); err != nil {
		t.Fatal(err)
	}
	th, _, _ = s.GetThread(ctx, "work", "th", false)
	if th.UnreadCount != 0 {
		t.Fatalf("unread = %d, want 0", th.UnreadCount)
	}

	// Moving a message to a different thread id updates both summaries.
	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "b", ThreadID: "th-other", Received: base.Add(time.Hour),
		MailboxRemotes: []string{"mb-inbox"},
	}, &mime.Parsed{Subject: "Re: Start", From: addr("B", "b@x.example"), TextBody: "two"})
	th, _, _ = s.GetThread(ctx, "work", "th", false)
	if th.MessageCount != 1 {
		t.Fatalf("old thread not shrunk: %+v", th)
	}
	other, _, err := s.GetThread(ctx, "work", "th-other", false)
	if err != nil {
		t.Fatalf("new thread missing: %v", err)
	}
	if other.MessageCount != 1 {
		t.Fatalf("new thread = %+v", other)
	}
}

// ---------------------------------------------------------------------------
// Search

func hitIDs(hits []SearchHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Message.RemoteID
	}
	return out
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedCorpus(t, s)

	cases := []struct {
		name   string
		query  string
		filter MessageFilter
		want   []string // as a set, order-insensitive unless noted
	}{
		{"single term", "invoice", MessageFilter{}, []string{"w1", "w3"}},
		{"case insensitive", "INVOICE", MessageFilter{}, []string{"w1", "w3"}},
		{"body term", "widgets", MessageFilter{}, []string{"w2", "w4"}},
		{"and", "invoice AND paid", MessageFilter{}, []string{"w3"}},
		{"or", "dinner OR digest", MessageFilter{}, []string{"p1", "w2"}},
		{"not", "widgets NOT older", MessageFilter{}, []string{"w2"}},
		{"phrase", `"total 1200"`, MessageFilter{}, []string{"w1"}},
		{"phrase misses reordering", `"1200 total"`, MessageFilter{}, nil},
		{"prefix", "invo*", MessageFilter{}, []string{"w1", "w3"}},
		{"prefix short", "di*", MessageFilter{}, []string{"p1", "w2"}},
		{"column subject", "subject:invoice", MessageFilter{}, []string{"w1", "w3"}},
		{"column from_name", "from_name:carol", MessageFilter{}, []string{"p1"}},
		{"column to", "to_json:dave", MessageFilter{}, []string{"w4"}},
		{"attachment name", `attachment_names:"invoice-august.pdf"`, MessageFilter{}, []string{"w1"}},
		{"attachment extension", "attachment_names:pdf", MessageFilter{}, []string{"w1"}},
		{"body only misses subject", "text_body:digest", MessageFilter{}, nil},
		{"with account filter", "invoice", MessageFilter{Accounts: []string{"personal"}}, nil},
		{"with mailbox filter", "invoice", MessageFilter{MailboxRole: "inbox"}, []string{"w1"}},
		{"with unread filter", "invoice", MessageFilter{Unread: ptrBool(true)}, []string{"w1"}},
		{"with since filter", "invoice", MessageFilter{Since: base.Add(time.Minute)}, []string{"w3"}},
		{"with nobulk", "widgets", MessageFilter{NoBulk: true}, []string{"w4"}},
		{"with from filter", "invoice", MessageFilter{From: "bob"}, []string{"w3"}},
		{"no hits", "zzzznotpresent", MessageFilter{}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := s.Search(ctx, tc.query, tc.filter)
			if err != nil {
				t.Fatalf("Search(%q): %v", tc.query, err)
			}
			got := map[string]bool{}
			for _, id := range hitIDs(hits) {
				got[id] = true
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Search(%q) = %v, want %v", tc.query, hitIDs(hits), tc.want)
			}
			for _, id := range tc.want {
				if !got[id] {
					t.Fatalf("Search(%q) = %v, want %v", tc.query, hitIDs(hits), tc.want)
				}
			}
			n, err := s.CountSearch(ctx, tc.query, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if n != len(hits) {
				t.Fatalf("CountSearch = %d, Search returned %d", n, len(hits))
			}
		})
	}
}

func TestSearchRankAndHighlight(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedCorpus(t, s)

	hits, err := s.Search(ctx, "invoice", MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("%d hits, want 2", len(hits))
	}
	// bm25 is negative; better matches sort first.
	if hits[0].Rank > hits[1].Rank {
		t.Fatalf("results not ordered by rank: %v > %v", hits[0].Rank, hits[1].Rank)
	}
	if hits[0].Rank >= 0 {
		t.Fatalf("rank = %v, want a negative bm25 score", hits[0].Rank)
	}
	// w1 says "invoice" twice in the body plus the subject; it must win.
	if hits[0].Message.RemoteID != "w1" {
		t.Fatalf("best hit = %s, want w1", hits[0].Message.RemoteID)
	}
	if hits[0].Highlight == "" {
		t.Fatal("no snippet returned")
	}
	if hits[0].Message.Subject == "" || len(hits[0].Message.MailboxRemotes) == 0 {
		t.Fatalf("hit message not fully populated: %+v", hits[0].Message)
	}
	if hits[0].Message.TextBody != "" {
		t.Error("search hits must not carry the full body")
	}

	marked, err := s.SearchHighlight(ctx, "invoice", MessageFilter{}, "[", "]")
	if err != nil {
		t.Fatal(err)
	}
	if len(marked) == 0 || marked[0].Highlight == "" {
		t.Fatal("no highlighted snippet")
	}
	if !contains(marked[0].Highlight, "[") || !contains(marked[0].Highlight, "]") {
		t.Fatalf("highlight markers missing: %q", marked[0].Highlight)
	}

	// Limit applies to the ranked list.
	top, err := s.Search(ctx, "invoice", MessageFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0].Message.RemoteID != "w1" {
		t.Fatalf("limited search = %v", hitIDs(top))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestSearchBadQuery(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedCorpus(t, s)

	for _, q := range []string{
		``,
		`   `,
		`"unbalanced quote`,
		`AND`,
		`nosuchcolumn:value`,
		`(unclosed`,
	} {
		if _, err := s.Search(ctx, q, MessageFilter{}); !errors.Is(err, ErrBadQuery) {
			t.Errorf("Search(%q) = %v, want ErrBadQuery", q, err)
		}
	}

	// QuoteFTS makes arbitrary text safe to search literally.
	safe := QuoteFTS(`"unbalanced quote`)
	if _, err := s.Search(ctx, safe, MessageFilter{}); err != nil {
		t.Fatalf("Search(QuoteFTS(...)) = %v, want no error", err)
	}
	hits, err := s.Search(ctx, QuoteFTS("Invoice 2026-08"), MessageFilter{})
	if err != nil {
		t.Fatalf("phrase search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("QuoteFTS phrase found %d, want 2", len(hits))
	}
}

func TestSearchFollowsEdits(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "r", ThreadID: "t", Received: base,
		MailboxRemotes: []string{"mb-inbox"},
	}, &mime.Parsed{Subject: "Original subject", TextBody: "aardvark content"})

	if hits, _ := s.Search(ctx, "aardvark", MessageFilter{}); len(hits) != 1 {
		t.Fatalf("term not indexed: %d hits", len(hits))
	}

	// Re-indexing with a new body replaces the FTS entry rather than adding one.
	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "r", ThreadID: "t", Received: base,
		MailboxRemotes: []string{"mb-inbox"},
	}, &mime.Parsed{Subject: "New subject", TextBody: "zebra content"})

	if hits, _ := s.Search(ctx, "aardvark", MessageFilter{}); len(hits) != 0 {
		t.Fatalf("stale term still indexed: %d hits", len(hits))
	}
	if hits, _ := s.Search(ctx, "zebra", MessageFilter{}); len(hits) != 1 {
		t.Fatalf("new term not indexed: %d hits", len(hits))
	}
	if hits, _ := s.Search(ctx, "subject:new", MessageFilter{}); len(hits) != 1 {
		t.Fatal("subject change not reflected in the index")
	}

	var ftsCount int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM messages_fts`).Scan(&ftsCount); err != nil {
		t.Fatal(err)
	}
	if ftsCount != 1 {
		t.Fatalf("messages_fts has %d rows for 1 message", ftsCount)
	}
	if report, err := s.IntegrityCheck(ctx); err != nil || report != "ok" {
		t.Fatalf("IntegrityCheck = %q %v", report, err)
	}
}

func TestSearchDiacriticsAndUnicode(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "u", ThreadID: "t", Received: base,
		MailboxRemotes: []string{"mb-inbox"},
	}, &mime.Parsed{
		Subject: "Réunion à Genève", From: addr("Émile Zola", "emile@example.fr"),
		TextBody: "Görüşürüz — tot ziens. Beträge: 100€",
	})

	for _, q := range []string{"reunion", "Réunion", "geneve", "emile", "betrage"} {
		hits, err := s.Search(ctx, q, MessageFilter{})
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		if len(hits) != 1 {
			t.Errorf("Search(%q) = %d hits, want 1 (diacritics should fold)", q, len(hits))
		}
	}
}

// TestListMessagesLargeResultSet exercises the chunking in attachMailboxes:
// an unbounded list must not run into SQLite's bound-variable limit.
func TestListMessagesLargeResultSet(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")

	const n = membershipChunk*2 + 37
	if err := s.Tx(ctx, func(tx *Tx) error {
		for i := range n {
			if _, err := tx.UpsertMessage(ctx, &model.Message{
				AccountID: "work", RemoteID: fmt.Sprintf("bulk-%04d", i),
				ThreadID: "th", Received: base.Add(time.Duration(i) * time.Second),
				MailboxRemotes: []string{"mb-inbox", "mb-work"},
			}, nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	msgs, err := s.ListMessages(ctx, MessageFilter{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != n {
		t.Fatalf("got %d messages, want %d", len(msgs), n)
	}
	for _, m := range msgs {
		if len(m.MailboxRemotes) != 2 {
			t.Fatalf("%s: MailboxRemotes = %v, want 2 entries", m.RemoteID, m.MailboxRemotes)
		}
	}
}
