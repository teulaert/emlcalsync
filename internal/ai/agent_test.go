package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// scripted is a client that plays back turns: each call returns the next
// one, and records the request it was given.
type scripted struct {
	turns []Message
	seen  []Request
}

func (s *scripted) client() Client {
	return Func{Name: "scripted", Run: func(ctx context.Context, req Request, emit func(string)) (Message, error) {
		s.seen = append(s.seen, req)
		if len(s.turns) == 0 {
			return Message{}, errors.New("script ran out")
		}
		m := s.turns[0]
		s.turns = s.turns[1:]
		if m.Content != "" {
			emit(m.Content)
		}
		m.Role = RoleAssistant
		return m, nil
	}}
}

// recorder is a toolset that answers every call with a fixed string and
// remembers what it was asked.
type recorder struct {
	calls  []ToolCall
	answer string
	err    error
}

func (r *recorder) Tools() []Tool {
	return []Tool{{Name: "mail_search", Description: "search", Parameters: json.RawMessage(`{"type":"object"}`)}}
}

func (r *recorder) Call(_ context.Context, c ToolCall) (string, error) {
	r.calls = append(r.calls, c)
	return r.answer, r.err
}

func call(name, args string) ToolCall {
	return ToolCall{Name: name, Arguments: json.RawMessage(args)}
}

func TestRunWithoutToolsIsOneCall(t *testing.T) {
	s := &scripted{turns: []Message{{Content: "Hoi Anna"}}}
	var streamed strings.Builder
	out, err := Run(context.Background(), s.client(), Request{Messages: []Message{{Role: RoleUser, Content: "write"}}}, nil,
		Observer{Text: func(x string) { streamed.WriteString(x) }})
	if err != nil || out != "Hoi Anna" || streamed.String() != "Hoi Anna" {
		t.Fatalf("out=%q streamed=%q err=%v", out, streamed.String(), err)
	}
	if len(s.seen) != 1 || s.seen[0].Tools != nil {
		t.Errorf("seen %d requests, tools=%v; want one with no tools", len(s.seen), s.seen[0].Tools)
	}
}

func TestRunLooksUpThenAnswers(t *testing.T) {
	s := &scripted{turns: []Message{
		{Content: "Let me check.", ToolCalls: []ToolCall{call("mail_search", `{"query":"offerte","from":"anna"}`)}},
		{Content: "Hoi Anna, de offerte van maart klopt."},
	}}
	tools := &recorder{answer: `[{"id":"work:1","subject":"offerte maart"}]`}
	var streamed strings.Builder
	var discards int
	var lookups, results []ToolCall
	out, err := Run(context.Background(), s.client(), Request{Messages: []Message{{Role: RoleUser, Content: "write"}}}, tools, Observer{
		Text:    func(x string) { streamed.WriteString(x) },
		Discard: func() { discards++; streamed.Reset() },
		Lookup:  func(c ToolCall) { lookups = append(lookups, c) },
		Result:  func(c ToolCall, r string, err error) { results = append(results, c) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "Hoi Anna, de offerte van maart klopt." || streamed.String() != out {
		t.Errorf("out=%q streamed=%q", out, streamed.String())
	}
	if discards != 1 {
		t.Errorf("the lead-in text should have been discarded once, got %d", discards)
	}
	if len(tools.calls) != 1 || tools.calls[0].Name != "mail_search" || string(tools.calls[0].Arguments) != `{"query":"offerte","from":"anna"}` {
		t.Errorf("tool calls = %+v", tools.calls)
	}
	if len(lookups) != 1 || len(results) != 1 {
		t.Errorf("observer saw %d lookups, %d results", len(lookups), len(results))
	}

	// The second request carries the whole exchange: user, the assistant's
	// ask, the tool's answer -- and the tools, still.
	if len(s.seen) != 2 {
		t.Fatalf("seen %d requests, want 2", len(s.seen))
	}
	second := s.seen[1]
	if len(second.Tools) != 1 {
		t.Errorf("second request lost the tools")
	}
	roles := make([]Role, 0, len(second.Messages))
	for _, m := range second.Messages {
		roles = append(roles, m.Role)
	}
	if want := []Role{RoleUser, RoleAssistant, RoleTool}; strings.Join(rolesOf(roles), ",") != strings.Join(rolesOf(want), ",") {
		t.Errorf("roles = %v, want %v", roles, want)
	}
	tr := second.Messages[2]
	if tr.ToolName != "mail_search" || !strings.Contains(tr.Content, "offerte maart") {
		t.Errorf("tool message = %+v", tr)
	}
	if len(second.Messages[1].ToolCalls) != 1 {
		t.Error("the assistant turn was not echoed back with its tool calls")
	}
}

func rolesOf(rs []Role) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}

func TestRunTellsTheModelAboutAToolError(t *testing.T) {
	s := &scripted{turns: []Message{
		{ToolCalls: []ToolCall{call("mail_search", `{"query":"("}`)}},
		{Content: "done"},
	}}
	tools := &recorder{err: errors.New("bad FTS query")}
	out, err := Run(context.Background(), s.client(), Request{}, tools, Observer{})
	if err != nil || out != "done" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if got := s.seen[1].Messages[1].Content; !strings.Contains(got, "error: bad FTS query") {
		t.Errorf("tool message = %q, want the error told back", got)
	}
}

func TestRunCutsALongResult(t *testing.T) {
	s := &scripted{turns: []Message{
		{ToolCalls: []ToolCall{call("mail_search", `{}`)}},
		{Content: "done"},
	}}
	tools := &recorder{answer: strings.Repeat("x", maxResultBytes*2)}
	if _, err := Run(context.Background(), s.client(), Request{}, tools, Observer{}); err != nil {
		t.Fatal(err)
	}
	got := s.seen[1].Messages[1].Content
	if len(got) > maxResultBytes+200 || !strings.Contains(got, "[cut:") {
		t.Errorf("result was not cut: %d bytes", len(got))
	}
}

func TestRunStopsLookingUpAtTheLimit(t *testing.T) {
	var turns []Message
	for i := 0; i < MaxLookups; i++ {
		turns = append(turns, Message{ToolCalls: []ToolCall{call("mail_search", `{}`)}})
	}
	turns = append(turns, Message{Content: "fine, here it is"})
	s := &scripted{turns: turns}
	tools := &recorder{answer: "[]"}
	out, err := Run(context.Background(), s.client(), Request{}, tools, Observer{})
	if err != nil || out != "fine, here it is" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(tools.calls) != MaxLookups {
		t.Errorf("%d lookups ran, want %d", len(tools.calls), MaxLookups)
	}
	last := s.seen[len(s.seen)-1]
	if last.Tools != nil {
		t.Error("the final request should offer no tools")
	}
	if m := last.Messages[len(last.Messages)-1]; m.Role != RoleUser || !strings.Contains(m.Content, "limit") {
		t.Errorf("the model was not told to stop: %+v", m)
	}
}

func TestRunGivesUpOnAModelThatWillNotWrite(t *testing.T) {
	var turns []Message
	for i := 0; i < MaxLookups+2; i++ {
		turns = append(turns, Message{ToolCalls: []ToolCall{call("mail_search", `{}`)}})
	}
	s := &scripted{turns: turns}
	_, err := Run(context.Background(), s.client(), Request{}, &recorder{answer: "[]"}, Observer{})
	if err == nil || !strings.Contains(err.Error(), "kept asking") {
		t.Errorf("err = %v", err)
	}
}

func TestRunStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &scripted{turns: []Message{
		{ToolCalls: []ToolCall{call("mail_search", `{}`), call("mail_search", `{}`)}},
		{Content: "never"},
	}}
	tools := &recorder{answer: "[]"}
	_, err := Run(ctx, s.client(), Request{}, tools, Observer{Lookup: func(ToolCall) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want Canceled", err)
	}
	if len(tools.calls) != 1 {
		t.Errorf("%d calls ran after cancel, want the loop to stop at 1", len(tools.calls))
	}
}

func TestReplyPromptExplainsLookups(t *testing.T) {
	with := ReplyPrompt(ReplyInput{Lookups: true}).Messages[0].Content
	without := ReplyPrompt(ReplyInput{}).Messages[0].Content
	if !strings.Contains(with, "look things up") || !strings.Contains(with, `"fastmail:t:abc"`) {
		t.Errorf("system prompt with lookups:\n%s", with)
	}
	if strings.Contains(without, "look things up") {
		t.Error("system prompt without lookups should not mention tools")
	}
}
