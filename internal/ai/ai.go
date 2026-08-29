// Package ai is the language-model layer: one small interface every surface
// talks to, and the backends behind it.
//
// It is deliberately narrow. A Client is one configured model that can be
// handed a conversation and streams text back; which model, where it runs and
// how it is spoken to are the backend's business (ai/ollama for now), chosen
// in internal/cli from the [ai] table the same way providers are chosen from
// an account's blocks. Nothing here touches the store, the engine or a
// provider — what a request is built out of is assembled by the caller, so
// the same prompt can be tested without a model on the other end and the
// same model can be driven from the TUI today and the CLI later.
//
// A model can also be given tools -- things it may ask to have done and be
// told the result of -- which is how it looks other mail up before writing.
// The Client only carries the calls back and forth; Run is the loop that
// executes them, and a Toolset is where they come from.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// Role is who said a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool is the result of a tool call, answering the assistant turn
	// that asked for it.
	RoleTool Role = "tool"
)

// Message is one turn of the conversation handed to the model.
type Message struct {
	Role    Role
	Content string
	// ToolCalls is what an assistant turn asked to have done, if anything.
	ToolCalls []ToolCall
	// ToolName and ToolCallID say which call a RoleTool message answers.
	// Backends differ in which they want; both are filled in.
	ToolName   string
	ToolCallID string
}

// Tool is something the model may ask for: a name, what it does, and a JSON
// schema for its arguments.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall is one request from the model. Arguments is a JSON object.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Request is one generation: the turns so far, the last of which is what the
// model answers, and the tools it may call.
type Request struct {
	Messages []Message
	Tools    []Tool
}

// Client is one configured model.
type Client interface {
	// Describe names the model for a status line: "qwen3:32b · ollama".
	Describe() string
	// ContextWindow is the window in tokens the model is run with, or 0 when
	// it is not known. Callers use it to budget what they put in a request.
	// Finding out may take a round trip to the server, so it is not for the
	// update loop.
	ContextWindow() int
	// Chat runs one turn: emit is called with each piece of answer text as
	// it arrives, on Chat's own goroutine, and the whole assistant turn --
	// the text again, plus any tool calls -- comes back once it is complete,
	// ctx is done, or the backend failed. Text handed to emit before an error
	// is still the model's; a partial answer is not undone.
	Chat(ctx context.Context, req Request, emit func(text string)) (Message, error)
}

// Toolset is what a model may call. Tools describes them; Call runs one and
// returns what the model is told.
type Toolset interface {
	Tools() []Tool
	Call(ctx context.Context, call ToolCall) (string, error)
}

// ErrUnavailable is wrapped by a backend that could not be reached at all, as
// opposed to one that was reached and said no. The distinction matters to a
// status line: the fix for one is starting a server, for the other reading
// what it said.
var ErrUnavailable = errors.New("model server unavailable")

// Func adapts a function to Client, for tests and for wrapping.
type Func struct {
	Name   string
	Window int
	Run    func(ctx context.Context, req Request, emit func(string)) (Message, error)
}

func (f Func) Describe() string   { return f.Name }
func (f Func) ContextWindow() int { return f.Window }
func (f Func) Chat(ctx context.Context, req Request, emit func(string)) (Message, error) {
	return f.Run(ctx, req, emit)
}

var (
	reThink = regexp.MustCompile(`(?s)^\s*<think>.*?</think>\s*`)
	reFence = regexp.MustCompile("(?s)^```[a-zA-Z]*\\n(.*?)\\n```\\s*$")
)

// CleanText tidies what a chat model hands back into text that can go into
// an editor as-is: a reasoning block a thinking model leaked into its answer,
// a code fence wrapped round the whole thing, and a "Subject:" line the
// prompt asked it not to write all come off. It is conservative on purpose —
// anything it does not recognise is the model's answer and stays.
func CleanText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = reThink.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if m := reFence.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[1])
	}
	if first, rest, ok := strings.Cut(s, "\n"); ok && strings.HasPrefix(strings.ToLower(first), "subject:") {
		s = strings.TrimSpace(rest)
	}
	return s
}
