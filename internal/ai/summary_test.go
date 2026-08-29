package ai

import (
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

func TestSummaryPromptHasTheShapeAndTheThread(t *testing.T) {
	thread := []model.Message{
		msg("m1", "anna", "Kun je dit bevestigen?", 2*time.Hour),
		msg("m2", "anna", "En graag voor vrijdag.", 0),
	}
	req := SummaryPrompt(SummaryInput{Self: model.Address{Email: "me@example.com"}, Thread: thread, Loc: time.UTC})
	sys, user := req.Messages[0].Content, req.Messages[1].Content
	for _, want := range []string{"About:", "Asked of you:", "Facts:", "Open:", "me@example.com"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if strings.Contains(sys, "look things up") {
		t.Error("no lookups were offered, yet the prompt mentions them")
	}
	if !strings.Contains(user, "Kun je dit bevestigen?") || !strings.Contains(user, "The newest message ---\nId: work:m2\nFrom: anna") {
		t.Errorf("user prompt:\n%s", user)
	}
	if strings.Contains(user, "The message being answered") {
		t.Error("a summary has no message being answered")
	}
	if !strings.Contains(user, "No question was asked") {
		t.Errorf("user prompt should ask for the summary:\n%s", user)
	}
}

func TestSummaryPromptAsksTheQuestionInstead(t *testing.T) {
	req := SummaryPrompt(SummaryInput{
		Self:     model.Address{Name: "Lennert", Email: "me@example.com"},
		Thread:   []model.Message{msg("m1", "anna", "de vraag", 0)},
		Question: "hebben we een prijs afgesproken?",
		Lookups:  true,
	})
	user := req.Messages[1].Content
	if !strings.Contains(user, "Question from Lennert <me@example.com>:\nhebben we een prijs afgesproken?") {
		t.Errorf("user prompt:\n%s", user)
	}
	if strings.Contains(user, "No question was asked") {
		t.Error("a question was asked")
	}
	if !strings.Contains(req.Messages[0].Content, "before answering") || !strings.Contains(req.Messages[0].Content, "An attachment can be read") {
		t.Errorf("lookups guidance should be worded for answering:\n%s", req.Messages[0].Content)
	}
}

func TestSummaryPromptSkipsDrafts(t *testing.T) {
	draft := msg("d1", "me", "half een antwoord", 0)
	draft.Flags.Draft = true
	req := SummaryPrompt(SummaryInput{Thread: []model.Message{msg("m1", "anna", "de vraag", time.Hour), draft}})
	if strings.Contains(req.Messages[1].Content, "half een antwoord") {
		t.Error("the draft should not be part of the conversation")
	}
}

// The model must not chat: the summary is read on a screen.
func TestSummaryPromptForbidsChatter(t *testing.T) {
	sys := SummaryPrompt(SummaryInput{}).Messages[0].Content
	for _, want := range []string{"no offer to do more", "no question back", "cannot open attachments"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
}
