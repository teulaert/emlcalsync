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
package ai

import (
	"context"
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
)

// Message is one turn of the conversation handed to the model.
type Message struct {
	Role    Role
	Content string
}

// Request is one generation: the turns so far, the last of which is what the
// model answers.
type Request struct {
	Messages []Message
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
	// Chat streams the answer to req: emit is called with each piece of text
	// as it arrives, on Chat's own goroutine, and Chat returns once the answer
	// is complete, ctx is done, or the backend failed. Text handed to emit
	// before an error is still the model's — a partial answer is not undone.
	Chat(ctx context.Context, req Request, emit func(text string)) error
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
	Run    func(ctx context.Context, req Request, emit func(string)) error
}

func (f Func) Describe() string   { return f.Name }
func (f Func) ContextWindow() int { return f.Window }
func (f Func) Chat(ctx context.Context, req Request, emit func(string)) error {
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
