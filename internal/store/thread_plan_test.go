package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A thread read must look the thread up, not walk the account. Without the
// pin, SQLite takes messages_account (account_id, received_utc) to save the
// sort, which reads every message of the account for each call; refreshThread
// runs once per updated message inside a write transaction, so a 40k-message
// envelope refresh held the write lock for hours and starved other accounts.
func TestThreadReadsUseTheThreadIndex(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s, "a")
	for i := 0; i < 50; i++ {
		m := threadFixture("a", fmt.Sprintf("m%d", i))
		m.ThreadID = fmt.Sprintf("t%d", i%10)
		if _, err := s.UpsertMessage(ctx, m, nil); err != nil {
			t.Fatalf("UpsertMessage: %v", err)
		}
	}

	for name, q := range map[string]string{
		"refreshThread":          threadSummarySQL,
		"GetThread":              threadMessagesSQL(false),
		"GetThread with deleted": threadMessagesSQL(true),
	} {
		rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, "a", "t1")
		if err != nil {
			t.Fatalf("%s: explain: %v", name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		got := strings.Join(plan, "; ")
		if !strings.Contains(got, "messages_thread (account_id=? AND thread_id=?)") {
			t.Errorf("%s: plan %q does not look the thread up by messages_thread", name, got)
		}
	}

	// And the pinned queries still answer: thread t1 holds m1, m11, ... m41.
	if got := len(mustThread(t, s, "a", "t1")); got != 5 {
		t.Fatalf("GetThread returned %d messages, want 5", got)
	}
}

func mustThread(t *testing.T, s *Store, acct, thread string) []string {
	t.Helper()
	_, msgs, err := s.GetThread(context.Background(), acct, thread, false)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	var ids []string
	for _, m := range msgs {
		ids = append(ids, m.RemoteID)
	}
	return ids
}
