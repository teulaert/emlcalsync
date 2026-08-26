package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/provider"
)

// watching reports whether a Pusher.Watch call is currently registered.
func (f *fakeMail) watching() bool {
	f.pushMu.Lock()
	defer f.pushMu.Unlock()
	return f.pushFn != nil
}

func (h *harness) indexed(remote string) bool {
	_, err := h.st.GetMessage(context.Background(), "work", remote)
	return err == nil
}

func TestWatchPushHintTriggersDelta(t *testing.T) {
	h := newHarness(t)
	h.cfg.Accounts[0].Push = true
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.eng.Watch(ctx) }()

	waitFor(t, "the initial sync", 10*time.Second, func() bool { return h.indexed("m1") })
	waitFor(t, "the push stream", 10*time.Second, h.mail.watching)

	h.mail.Add(&fakeMsg{id: "m2", raw: mailRaw(t, "two", "two")})
	h.mail.Push(provider.ChangeHint{Mail: true})

	waitFor(t, "the pushed message", 10*time.Second, func() bool { return h.indexed("m2") })

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Watch did not return after cancellation")
	}
}

func TestWatchNudgeTriggersPass(t *testing.T) {
	h := newHarness(t)
	h.cfg.Accounts[0].Push = false
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.eng.Watch(ctx) }()

	waitFor(t, "the initial sync", 10*time.Second, func() bool { return h.indexed("m1") })

	h.mail.Add(&fakeMsg{id: "m2", raw: mailRaw(t, "two", "two")})
	h.eng.Nudge()
	waitFor(t, "the nudged pass", 10*time.Second, func() bool { return h.indexed("m2") })

	cancel()
	<-done
}

func TestWatchSurvivesOfflineProvider(t *testing.T) {
	h := newHarness(t)
	h.cfg.Accounts[0].Push = false
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.mail.FailNext(1) // the first provider call of the initial pass fails

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.eng.Watch(ctx) }()

	// The daemon must stay alive and pick the account up on the next nudge.
	waitFor(t, "the first pass to fail", 10*time.Second, func() bool {
		h.mail.mu.Lock()
		defer h.mail.mu.Unlock()
		return h.mail.failNext == 0
	})
	h.eng.Nudge()
	waitFor(t, "recovery", 30*time.Second, func() bool { return h.indexed("m1") })

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Watch did not return")
	}
}

func TestWatchRefusesALockedAccount(t *testing.T) {
	h := newHarness(t)
	release, err := h.eng.lockAccount("work")
	if err != nil {
		t.Fatalf("lockAccount: %v", err)
	}
	defer release()

	other := h.newEngine()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := other.Watch(ctx); !errors.Is(err, ErrLocked) {
		t.Fatalf("Watch = %v, want ErrLocked", err)
	}
}

func TestDebounceCoalescesHints(t *testing.T) {
	w := newAccountWatch()
	w.fire(provider.ChangeHint{Mail: true})
	w.fire(provider.ChangeHint{Calendar: true})
	h := w.take()
	if !h.Mail || !h.Calendar {
		t.Fatalf("hint = %+v, want both resources merged", h)
	}
	if again := w.take(); !again.Mail || !again.Calendar {
		t.Fatalf("an empty hint must mean everything, got %+v", again)
	}
}
