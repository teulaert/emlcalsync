package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/ai"
)

// summaryModel answers with a fixed text and remembers every request.
type summaryModel struct {
	answer string
	seen   []ai.Request
}

func (m *summaryModel) client() ai.Client {
	return ai.Func{Name: "fake-model · test", Run: func(ctx context.Context, req ai.Request, emit func(string)) (ai.Message, error) {
		m.seen = append(m.seen, req)
		emit(m.answer)
		return ai.Message{Role: ai.RoleAssistant, Content: m.answer}, nil
	}}
}

func summaryOn(t *testing.T, r *root) *summaryView {
	t.Helper()
	s, ok := r.top().(*summaryView)
	if !ok {
		t.Fatalf("top = %T, want the summary screen", r.top())
	}
	return s
}

func TestSummaryNeedsAModel(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	r := newTestRoot(t, d)
	send(t, r, "ctrl+g")
	if _, ok := r.top().(*mailList); !ok || !strings.Contains(r.render(), "no AI model configured") {
		t.Errorf("top = %T; render:\n%s", r.top(), r.render())
	}
}

func TestSummaryFromTheList(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	addMessage(t, d, "work", "w2", "t1", "Re: offerte Q4", "bob", 30*time.Minute, false)
	m := &summaryModel{answer: "About: de offerte voor Q4.\nAsked of you: bevestigen.\nFacts: 400 stuks.\nOpen: nothing"}
	d.AI = m.client()

	r := newTestRoot(t, d)
	send(t, r, "ctrl+g")
	s := summaryOn(t, r)
	if !s.asking || !strings.Contains(r.render(), "enter alone for a summary") {
		t.Fatalf("the prompt did not open:\n%s", r.render())
	}
	send(t, r, "enter")

	if s.run != nil || s.err != nil {
		t.Fatalf("run=%v err=%v", s.run != nil, s.err)
	}
	if s.text != m.answer {
		t.Errorf("text = %q", s.text)
	}
	if !strings.Contains(r.render(), "Asked of you: bevestigen.") || !strings.Contains(r.render(), "summary · offerte Q4") {
		t.Errorf("render:\n%s", r.render())
	}
	if !strings.Contains(r.render(), "r reply · ctrl+g ask again") {
		t.Errorf("footer:\n%s", r.render())
	}
	// The model saw the whole thread and was asked for the shape.
	user := m.seen[0].Messages[1].Content
	for _, want := range []string{"Kun je dit bevestigen?", "Re: offerte Q4 body", "No question was asked"} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt lacks %q:\n%s", want, user)
		}
	}
	if !strings.Contains(m.seen[0].Messages[0].Content, "work@example.com") {
		t.Error("the summary is not for the account's address")
	}

	// r from the summary answers the thread: the composer opens on its
	// newest message.
	send(t, r, "r")
	c := composerOn(t, r)
	if c.orig == nil || c.orig.RemoteID != "w2" {
		t.Errorf("composer opened on %v, want w2", c.orig)
	}
	send(t, r, "esc") // untouched composer closes at once
	summaryOn(t, r)

	// esc leaves the summary.
	send(t, r, "esc")
	if _, ok := r.top().(*mailList); !ok {
		t.Errorf("esc did not go back to the list: %T", r.top())
	}

	// Coming back is free: the answer is remembered for the thread as it
	// stands.
	send(t, r, "ctrl+g")
	send(t, r, "enter")
	if len(m.seen) != 1 {
		t.Errorf("the model was asked %d times, want once", len(m.seen))
	}
	if s := summaryOn(t, r); s.text != m.answer {
		t.Errorf("cached text = %q", s.text)
	}
	send(t, r, "esc")

	// A new message in the thread is a new conversation.
	addMessage(t, d, "work", "w3", "t1", "Re: offerte Q4", "anna", time.Minute, true)
	send(t, r, "ctrl+g")
	send(t, r, "enter")
	if len(m.seen) != 2 {
		t.Errorf("a changed thread should be asked about again, got %d asks", len(m.seen))
	}
}

func TestSummaryAnswersAQuestion(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	m := &summaryModel{answer: "Nee, er is geen prijs genoemd (bericht van Anna)."}
	d.AI = m.client()

	r := newTestRoot(t, d)
	send(t, r, "r") // not the composer's ctrl+g: go via the thread
	send(t, r, "esc")
	send(t, r, "enter") // open the thread
	if _, ok := r.top().(*threadView); !ok {
		t.Fatalf("top = %T", r.top())
	}
	send(t, r, "ctrl+g")
	typeInto(t, r, "is er een prijs afgesproken?")
	send(t, r, "enter")

	s := summaryOn(t, r)
	if s.question != "is er een prijs afgesproken?" || s.text != m.answer {
		t.Errorf("question=%q text=%q", s.question, s.text)
	}
	if !strings.Contains(r.render(), "answer · offerte Q4") {
		t.Errorf("title should say it is an answer:\n%s", r.render())
	}
	if !strings.Contains(m.seen[0].Messages[1].Content, "Question from work@example.com:\nis er een prijs afgesproken?") {
		t.Errorf("prompt:\n%s", m.seen[0].Messages[1].Content)
	}

	// ctrl+g on the answer asks again; the summary is a separate answer.
	send(t, r, "ctrl+g")
	if !s.asking {
		t.Fatal("ctrl+g on the summary should reopen the prompt")
	}
	send(t, r, "enter")
	if len(m.seen) != 2 || s.question != "" {
		t.Errorf("asks=%d question=%q", len(m.seen), s.question)
	}
}

func TestSummaryPromptEscGoesBackWhenNothingWasAsked(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	m := &summaryModel{answer: "x"}
	d.AI = m.client()
	r := newTestRoot(t, d)
	send(t, r, "ctrl+g")
	summaryOn(t, r)
	send(t, r, "esc")
	if _, ok := r.top().(*mailList); !ok || len(m.seen) != 0 {
		t.Errorf("top = %T, asks = %d", r.top(), len(m.seen))
	}
}

func TestSummaryLooksThingsUp(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	tools := &lookupToolset{}
	d.Tools = tools
	var calls int
	d.AI = ai.Func{Name: "agent", Run: func(ctx context.Context, req ai.Request, emit func(string)) (ai.Message, error) {
		calls++
		if calls == 1 {
			return ai.Message{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{
				{Name: "mail_search", Arguments: json.RawMessage(`{"query":"offerte"}`)},
			}}, nil
		}
		emit("About: Q4, zelfde prijs als maart (12,50).")
		return ai.Message{Role: ai.RoleAssistant, Content: "About: Q4, zelfde prijs als maart (12,50)."}, nil
	}}
	r := newTestRoot(t, d)
	send(t, r, "ctrl+g")
	send(t, r, "enter")
	s := summaryOn(t, r)
	if len(tools.calls) != 1 || !strings.Contains(s.text, "12,50") {
		t.Errorf("lookups=%d text=%q", len(tools.calls), s.text)
	}
}

func TestSummaryEscStopsARun(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	release := make(chan struct{})
	d.AI = ai.Func{Name: "slow", Run: func(ctx context.Context, req ai.Request, emit func(string)) (ai.Message, error) {
		emit("About: ")
		select {
		case <-release:
			return ai.Message{Role: ai.RoleAssistant, Content: "About: x"}, nil
		case <-ctx.Done():
			return ai.Message{}, ctx.Err()
		}
	}}
	r := newTestRoot(t, d)
	send(t, r, "ctrl+g")
	s := summaryOn(t, r)
	// Drive by hand, as the composer test does.
	_, cmd := r.Update(keyPress("enter"))
	msgs := cmdBatch(t, cmd)
	done := make(chan struct{})
	go func() { msgs[0](); close(done) }()
	first := msgs[1]()
	r.Update(first)
	if s.run == nil || !strings.Contains(r.render(), "asking slow…") || !strings.Contains(r.render(), "About: ") {
		t.Fatalf("after the first chunk: run=%v\n%s", s.run != nil, r.render())
	}
	send(t, r, "esc")
	if s.run != nil || !strings.Contains(r.render(), "stopped") {
		t.Errorf("esc did not stop: run=%v\n%s", s.run != nil, r.render())
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not return after cancel")
	}
	close(release)
}
