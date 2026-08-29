package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Observer is told what Run is doing, so a status line can say so. Every
// field is optional.
type Observer struct {
	// Text is a piece of the answer, as it arrives.
	Text func(text string)
	// Discard says the text streamed so far was a lead-in to a lookup, not
	// the answer: the model asked for tools after it, and answers again once
	// it has the results.
	Discard func()
	// Lookup is a tool call about to run.
	Lookup func(call ToolCall)
	// Result is what a tool call came back with. err is the tool's own
	// failure, which the model is told about and may work around.
	Result func(call ToolCall, result string, err error)
}

const (
	// MaxLookups bounds a generation: after this many tool calls the model
	// is told to write with what it has. A local model can otherwise search
	// forever for a certainty the archive does not hold.
	MaxLookups = 8
	// maxResultBytes caps what one lookup can put into the conversation. A
	// listing that long says the query was too broad; the model sees the
	// start and is told the rest was cut.
	maxResultBytes = 24 * 1024
	// maxTurns bounds the loop outright, whatever the model does.
	maxTurns = MaxLookups + 2
)

// Run drives a generation to its answer: the model is called, whatever it
// asks to have looked up is looked up and told back, and it is called again,
// until it answers in text. With a nil Toolset it is one call, exactly as
// Chat. The answer is the last assistant turn's text, untouched.
func Run(ctx context.Context, c Client, req Request, tools Toolset, obs Observer) (string, error) {
	if tools != nil {
		req.Tools = tools.Tools()
	}
	lookups := 0
	for turn := 0; turn < maxTurns; turn++ {
		msg, err := c.Chat(ctx, req, obs.text)
		if err != nil {
			return "", err
		}
		if len(msg.ToolCalls) == 0 || tools == nil {
			return msg.Content, nil
		}
		if req.Tools == nil {
			// It was told to stop looking things up and asked anyway. Its
			// text, if it wrote any, is the best answer there is.
			if strings.TrimSpace(msg.Content) != "" {
				return msg.Content, nil
			}
			return "", errors.New("the model kept asking for lookups instead of writing")
		}
		obs.discard()
		req.Messages = append(req.Messages, msg)
		for _, call := range msg.ToolCalls {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			obs.lookup(call)
			out, err := tools.Call(ctx, call)
			obs.result(call, out, err)
			if err != nil {
				out = "error: " + err.Error()
			}
			if len(out) > maxResultBytes {
				out = out[:maxResultBytes] + "\n[cut: the result was too long -- narrow the query]"
			}
			req.Messages = append(req.Messages, Message{
				Role: RoleTool, Content: out, ToolName: call.Name, ToolCallID: call.ID,
			})
			lookups++
		}
		if lookups >= MaxLookups {
			req.Tools = nil
			req.Messages = append(req.Messages, Message{
				Role:    RoleUser,
				Content: fmt.Sprintf("That is %d lookups, which is the limit. Write the reply now with what you have.", lookups),
			})
		}
	}
	return "", errors.New("the model did not arrive at an answer")
}

func (o Observer) text(s string) {
	if o.Text != nil {
		o.Text(s)
	}
}

func (o Observer) discard() {
	if o.Discard != nil {
		o.Discard()
	}
}

func (o Observer) lookup(c ToolCall) {
	if o.Lookup != nil {
		o.Lookup(c)
	}
}

func (o Observer) result(c ToolCall, r string, err error) {
	if o.Result != nil {
		o.Result(c, r, err)
	}
}
