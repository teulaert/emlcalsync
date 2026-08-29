package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/ai"
)

// scriptedAI is a model that answers with a fixed set of chunks and keeps
// the request it was asked, so a test can see what the composer sent.
type scriptedAI struct {
	chunks []string
	err    error
	got    *ai.Request
	calls  int
}

func (s *scriptedAI) client() ai.Client {
	return ai.Func{
		Name:   "fake-model · test",
		Window: 8192,
		Run: func(ctx context.Context, req ai.Request, emit func(string)) error {
			s.calls++
			r := req
			s.got = &r
			for _, c := range s.chunks {
				emit(c)
			}
			return s.err
		},
	}
}

func (s *scriptedAI) user(t *testing.T) string {
	t.Helper()
	if s.got == nil {
		t.Fatal("the model was never asked")
	}
	return s.got.Messages[len(s.got.Messages)-1].Content
}

func typeInto(t *testing.T, r *root, text string) {
	t.Helper()
	for _, ch := range text {
		send(t, r, string(ch))
	}
}

func TestAIDraftNeedsAModel(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)

	send(t, r, "ctrl+g")
	if c.asking {
		t.Error("the prompt opened with no model configured")
	}
	if !errors.Is(c.err, errNoAI) || !strings.Contains(r.render(), "no AI model configured") {
		t.Errorf("err = %v; render:\n%s", c.err, r.render())
	}
}

func TestAIDraftFillsTheBodyAboveTheQuote(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	addMessage(t, d, "work", "w2", "t1", "Re: offerte Q4", "bob", 30*time.Minute, false)
	fake := &scriptedAI{chunks: []string{"Hoi Anna,\n\n", "Ja, ", "bij deze bevestigd.", "\n\nGroet"}}
	d.AI = fake.client()

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	quote := c.quote
	if quote == "" || !strings.Contains(c.body.Value(), quote) {
		t.Fatalf("the composer did not open on the quote: %q", c.body.Value())
	}

	send(t, r, "ctrl+g")
	if !c.asking {
		t.Fatalf("ctrl+g did not open the prompt: %s", r.render())
	}
	if !strings.Contains(r.render(), "instructions") {
		t.Errorf("the prompt is not on the status line:\n%s", r.render())
	}
	typeInto(t, r, "kort en vriendelijk")
	send(t, r, "enter")

	if c.assist != nil || c.asking {
		t.Fatalf("the generation did not finish: assist=%v asking=%v err=%v", c.assist != nil, c.asking, c.err)
	}
	if c.err != nil {
		t.Fatalf("err = %v", c.err)
	}
	want := "Hoi Anna,\n\nJa, bij deze bevestigd.\n\nGroet\n\n" + quote
	if c.body.Value() != want {
		t.Errorf("body =\n%q\nwant\n%q", c.body.Value(), want)
	}
	if c.body.Line() != 0 {
		t.Errorf("cursor on line %d, want the top so the draft is read first", c.body.Line())
	}
	if !strings.Contains(r.render(), "drafted by fake-model") {
		t.Errorf("the status line does not say who wrote it:\n%s", r.render())
	}

	// What the model was asked: the whole thread, the instructions, no
	// "written so far" since nothing was.
	user := fake.user(t)
	for _, want := range []string{"Kun je dit bevestigen?", "Re: offerte Q4 body", "kort en vriendelijk"} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt lacks %q:\n%s", want, user)
		}
	}
	if strings.Contains(user, "had started writing") {
		t.Errorf("nothing was written, yet the prompt says so:\n%s", user)
	}
	if !strings.Contains(fake.got.Messages[0].Content, "work@example.com") {
		t.Errorf("system prompt does not name the sender:\n%s", fake.got.Messages[0].Content)
	}

	// It is work now: esc asks before throwing it away.
	send(t, r, "esc")
	if _, still := r.top().(*composeView); !still || c.pending != pendingDiscard {
		t.Error("a generated draft should count as work to lose")
	}
}

func TestAIDraftWithNoInstructionsJustAnswers(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	fake := &scriptedAI{chunks: []string{"Bevestigd."}}
	d.AI = fake.client()

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	send(t, r, "ctrl+g")
	send(t, r, "enter")

	if !strings.HasPrefix(c.body.Value(), "Bevestigd.\n\n") {
		t.Errorf("body = %q", c.body.Value())
	}
	if !strings.Contains(fake.user(t), "gave no instructions") {
		t.Errorf("prompt:\n%s", fake.user(t))
	}
}

func TestAIDraftAsksBeforeReplacingWhatWasWritten(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	fake := &scriptedAI{chunks: []string{"Hoi Anna, ja hoor."}}
	d.AI = fake.client()

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	typeInto(t, r, "Hoi Anna, ja")

	send(t, r, "ctrl+g")
	if c.asking || c.pending != pendingReplace {
		t.Fatalf("ctrl+g over written text should ask first: asking=%v pending=%v", c.asking, c.pending)
	}
	if !strings.Contains(r.render(), "replaces what you wrote") {
		t.Errorf("render:\n%s", r.render())
	}
	// A letter typed in between is a change of mind.
	send(t, r, "!")
	if c.pending != pendingNone {
		t.Error("typing should lapse the question")
	}
	send(t, r, "ctrl+g")
	send(t, r, "ctrl+g")
	if !c.asking {
		t.Fatal("the second ctrl+g should open the prompt")
	}
	send(t, r, "enter")

	if !strings.HasPrefix(c.body.Value(), "Hoi Anna, ja hoor.\n\n"+c.quote[:10]) {
		t.Errorf("body = %q", c.body.Value())
	}
	if user := fake.user(t); !strings.Contains(user, "had started writing") || !strings.Contains(user, "Hoi Anna, ja!") {
		t.Errorf("what was written should be in the prompt:\n%s", user)
	}
}

func TestAIDraftRestoresTheBodyOnError(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	fake := &scriptedAI{chunks: []string{"Hoi"}, err: errors.New("ollama: model 'x' not found")}
	d.AI = fake.client()

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	before := c.body.Value()
	send(t, r, "ctrl+g")
	send(t, r, "enter")

	if c.body.Value() != before {
		t.Errorf("body = %q, want it put back as it was", c.body.Value())
	}
	if c.err == nil || !strings.Contains(r.render(), "not found") {
		t.Errorf("err = %v; render:\n%s", c.err, r.render())
	}
	if c.assist != nil {
		t.Error("the generation should be over")
	}
}

func TestAIDraftEscStopsAndRestores(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	release := make(chan struct{})
	var calls int
	d.AI = ai.Func{Name: "slow", Run: func(ctx context.Context, req ai.Request, emit func(string)) error {
		calls++
		emit("Hoi ")
		select {
		case <-release:
			emit("Anna")
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	before := c.body.Value()
	send(t, r, "ctrl+g")

	// Drive the generation by hand: the run and the first read are batched.
	_, cmd := r.Update(keyPress("enter"))
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("enter did not start a run and a read: %T", cmd())
	}
	done := make(chan struct{})
	go func() { batch[0](); close(done) }()
	first := batch[1]()
	_, cmd = r.Update(first)
	if c.assist == nil || c.body.Value() != "Hoi " {
		t.Fatalf("after the first chunk: assist=%v body=%q", c.assist != nil, c.body.Value())
	}
	if !strings.Contains(r.render(), "drafting with slow") || !strings.Contains(r.render(), "esc to stop") {
		t.Errorf("render:\n%s", r.render())
	}
	// Typing while it arrives is ignored; esc stops it.
	send(t, r, "x")
	if c.body.Value() != "Hoi " {
		t.Errorf("a key during generation changed the body: %q", c.body.Value())
	}
	send(t, r, "esc")
	if c.assist != nil || c.body.Value() != before {
		t.Errorf("esc did not stop and restore: assist=%v body=%q", c.assist != nil, c.body.Value())
	}
	if !strings.Contains(r.render(), "draft stopped") {
		t.Errorf("render:\n%s", r.render())
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not return after cancel")
	}
	// The read that was pending drains to nothing and changes nothing.
	if msg := cmd(); msg != nil {
		r.Update(msg)
	}
	if c.body.Value() != before {
		t.Errorf("a straggler changed the body: %q", c.body.Value())
	}
	close(release)
}

// A stored draft has no quote of its own to point at; the split falls back
// to finding where quoting starts, so the quote survives the redraft.
func TestAIDraftOnAStoredDraftKeepsItsQuote(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	addDraft(t, d, "work", "w2", "t1", "Re: offerte Q4", 0)
	// Give the draft a body with a quote under some half-written text.
	m, err := d.Store.GetMessage(context.Background(), "work", "w2")
	if err != nil {
		t.Fatal(err)
	}
	m.TextBody = "Hoi Anna,\n\nIk\n\nOp ma 25 aug. 2026 om 11:00 schreef Anna de Vries <anna@example.com>:\n> Kun je dit bevestigen?"
	if _, err := d.Store.UpsertMessage(context.Background(), m, nil); err != nil {
		t.Fatal(err)
	}
	fake := &scriptedAI{chunks: []string{"Hoi Anna,\n\nIk bevestig het."}}
	d.AI = fake.client()

	r := newTestRoot(t, d)
	send(t, r, "r") // carries on the draft
	c := composerOn(t, r)
	if c.draftRemote != "w2" {
		t.Fatalf("editing %q, want the draft", c.draftRemote)
	}
	send(t, r, "ctrl+g")
	send(t, r, "ctrl+g") // there is text above the quote
	send(t, r, "enter")

	want := "Hoi Anna,\n\nIk bevestig het.\n\nOp ma 25 aug. 2026 om 11:00 schreef Anna de Vries <anna@example.com>:\n> Kun je dit bevestigen?"
	if c.body.Value() != want {
		t.Errorf("body =\n%q\nwant\n%q", c.body.Value(), want)
	}
	// The draft itself is not part of the conversation shown to the model,
	// but the message it answers is, and so is the half-written text.
	user := fake.user(t)
	if !strings.Contains(user, "Kun je dit bevestigen?") || !strings.Contains(user, "had started writing this") {
		t.Errorf("prompt:\n%s", user)
	}
	if strings.Contains(user, "--- Message ---\nFrom: work") {
		t.Errorf("the draft is shown as a message in the thread:\n%s", user)
	}
}

func TestAIDraftPromptEscCloses(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	fake := &scriptedAI{chunks: []string{"x"}}
	d.AI = fake.client()

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	send(t, r, "ctrl+g")
	typeInto(t, r, "abc")
	send(t, r, "backspace")
	if c.instr != "ab" {
		t.Errorf("instr = %q", c.instr)
	}
	send(t, r, "esc")
	if c.asking || fake.calls != 0 {
		t.Errorf("esc should close the prompt without asking the model: asking=%v calls=%d", c.asking, fake.calls)
	}
	if _, still := r.top().(*composeView); !still {
		t.Error("esc in the prompt closed the composer")
	}
}
