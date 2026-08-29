package ai

import (
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

var testNow = time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC)

func msg(remote, from, body string, ago time.Duration) model.Message {
	return model.Message{
		AccountID: "work",
		RemoteID:  remote,
		ThreadID:  "t1",
		Subject:   "offerte Q4",
		From:      model.Address{Name: from, Email: from + "@example.com"},
		To:        []model.Address{{Email: "me@example.com"}},
		Date:      testNow.Add(-ago),
		TextBody:  body,
	}
}

func TestReplyPromptLaysOutTheThread(t *testing.T) {
	thread := []model.Message{
		msg("m1", "anna", "Kun je dit bevestigen?", 2*time.Hour),
		msg("m2", "me", "Ik kijk er vanmiddag naar.\n\nOp eerder schreef Anna:\n> Kun je dit bevestigen?", time.Hour),
		msg("m3", "anna", "Graag, het moet vandaag de deur uit.", 0),
	}
	req := ReplyPrompt(ReplyInput{
		Self:         model.Address{Email: "me@example.com"},
		Thread:       thread,
		Instructions: "zeg dat het goed is en dat ik morgen terugkom op de prijs",
		Loc:          time.UTC,
	})
	if len(req.Messages) != 2 || req.Messages[0].Role != RoleSystem || req.Messages[1].Role != RoleUser {
		t.Fatalf("messages = %+v, want system then user", req.Messages)
	}
	sys, user := req.Messages[0].Content, req.Messages[1].Content
	if !strings.Contains(sys, "me@example.com") {
		t.Errorf("system prompt does not say who it writes as:\n%s", sys)
	}
	for _, want := range []string{
		"Kun je dit bevestigen?",
		"Ik kijk er vanmiddag naar.",
		"Graag, het moet vandaag de deur uit.",
		"zeg dat het goed is",
		"From: anna <anna@example.com>",
		"Date: Tue 25 Aug 2026 09:00", // m1, two hours before testNow
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt lacks %q:\n%s", want, user)
		}
	}
	// The quoted tail of m2 is stripped: the thread already holds m1.
	if strings.Contains(user, "> Kun je dit bevestigen?") {
		t.Errorf("quoted material was not stripped:\n%s", user)
	}
	// Order is oldest first, and the newest is marked as the one answered.
	i1 := strings.Index(user, "Kun je dit bevestigen?")
	i3 := strings.Index(user, "Graag, het moet")
	if i1 > i3 {
		t.Error("thread is not oldest first")
	}
	if marked := strings.Index(user, "The message being answered"); marked < 0 || marked > i3 || marked < i1 {
		t.Errorf("the newest message is not the one marked as answered:\n%s", user)
	}
	if strings.Count(user, "The message being answered") != 1 {
		t.Errorf("exactly one message should be marked as answered:\n%s", user)
	}
}

func TestReplyPromptWithNoInstructionsSaysSo(t *testing.T) {
	req := ReplyPrompt(ReplyInput{Thread: []model.Message{msg("m1", "anna", "hoi", 0)}})
	if !strings.Contains(req.Messages[1].Content, "gave no instructions") {
		t.Errorf("user prompt:\n%s", req.Messages[1].Content)
	}
}

func TestReplyPromptCarriesWhatWasWritten(t *testing.T) {
	req := ReplyPrompt(ReplyInput{
		Thread:  []model.Message{msg("m1", "anna", "hoi", 0)},
		Written: "Hoi Anna, ja dat is",
	})
	if !strings.Contains(req.Messages[1].Content, "Hoi Anna, ja dat is") {
		t.Errorf("what was written so far is not in the prompt:\n%s", req.Messages[1].Content)
	}
}

func TestReplyPromptSkipsDraftsAndKeepsTheAnswered(t *testing.T) {
	draft := msg("d1", "me", "half een antwoord", 0)
	draft.Flags.Draft = true
	orig := msg("m1", "anna", "de vraag", time.Hour)
	req := ReplyPrompt(ReplyInput{
		Thread:    []model.Message{orig, draft},
		Answering: &orig,
	})
	user := req.Messages[1].Content
	if strings.Contains(user, "half een antwoord") {
		t.Errorf("a draft in the thread should not be shown as part of the exchange:\n%s", user)
	}
	if !strings.Contains(user, "The message being answered ---\nFrom: anna") {
		t.Errorf("the message answered is not marked:\n%s", user)
	}
}

// A composer may only have the one message in hand; it still reads as a
// conversation of one.
func TestReplyPromptWithOnlyTheAnsweredMessage(t *testing.T) {
	orig := msg("m1", "anna", "de vraag", 0)
	req := ReplyPrompt(ReplyInput{Answering: &orig})
	if !strings.Contains(req.Messages[1].Content, "de vraag") {
		t.Errorf("user prompt:\n%s", req.Messages[1].Content)
	}
}

func TestReplyPromptTrimsOldMessagesToTheWindow(t *testing.T) {
	long := strings.Repeat("lorem ipsum dolor sit amet ", 200) // ~5.4k chars
	thread := []model.Message{
		msg("m1", "anna", "eerste "+long, 3*time.Hour),
		msg("m2", "bob", "tweede "+long, 2*time.Hour),
		msg("m3", "anna", "derde, de vraag", 0),
	}
	// A window of 4000 tokens leaves (4000-1500)*3 = 7500 chars: room for two
	// messages, not three. (A window this small is only ever a test's.)
	req := ReplyPrompt(ReplyInput{Thread: thread, ContextWindow: 4000})
	user := req.Messages[1].Content
	if strings.Contains(user, "eerste") {
		t.Error("the oldest message should have been dropped")
	}
	if !strings.Contains(user, "tweede") || !strings.Contains(user, "derde, de vraag") {
		t.Errorf("the newer messages should be kept:\n%s", user[:min(len(user), 400)])
	}
	if !strings.Contains(user, "[1 earlier message(s) omitted]") {
		t.Error("the omission should be said")
	}
}

func TestReplyPromptShortensAnAnsweredMessageThatAloneIsTooLong(t *testing.T) {
	huge := "BEGIN " + strings.Repeat("x", 30000) + " END, the ask is here"
	req := ReplyPrompt(ReplyInput{Thread: []model.Message{msg("m1", "anna", huge, 0)}, ContextWindow: 2000})
	user := req.Messages[1].Content
	if !strings.Contains(user, "BEGIN") || !strings.Contains(user, "the ask is here") {
		t.Error("both ends of the answered message should survive")
	}
	if !strings.Contains(user, "[…]") {
		t.Error("the cut should be marked")
	}
	if len(user) > 4000 {
		t.Errorf("prompt is %d chars, want it cut to roughly the budget", len(user))
	}
}

func TestCleanText(t *testing.T) {
	cases := map[string]string{
		"  Hoi Anna,\n\nJa.\n":                              "Hoi Anna,\n\nJa.",
		"<think>\nlet me think\n</think>\n\nHoi Anna":       "Hoi Anna",
		"```\nHoi Anna,\nJa.\n```":                          "Hoi Anna,\nJa.",
		"```text\nHoi Anna\n```\n":                          "Hoi Anna",
		"Subject: Re: offerte\n\nHoi Anna":                  "Hoi Anna",
		"Hoi Anna,\r\n\r\nJa.":                              "Hoi Anna,\n\nJa.",
		"Hoi Anna, see `code` and ```not a whole fence```.": "Hoi Anna, see `code` and ```not a whole fence```.",
	}
	for in, want := range cases {
		if got := CleanText(in); got != want {
			t.Errorf("CleanText(%q) = %q, want %q", in, got, want)
		}
	}
}
