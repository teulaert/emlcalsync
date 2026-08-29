package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/ai"
	"github.com/teulaert/emlcalsync/internal/mime"
)

// The composer's AI draft: ctrl+g asks for instructions on the status line,
// enter hands the thread and the instructions to the configured model, and
// the answer streams into the body above the quote, replacing whatever was
// written there. The person then edits and sends it like anything else they
// typed -- nothing about a draft the model wrote is special once it is in
// the editor.
//
// The model call runs where every other blocking call runs: in a tea.Cmd,
// off the update loop. It is the one long-running one, so it is cancellable
// (esc) and its output arrives piecemeal, each piece a draftEvent read off a
// channel by the next Cmd in the chain. The events carry the seq of the
// generation they belong to, so a stopped generation's stragglers are
// ignored, the same way stale loads are everywhere else.

// draftEvent is one step of a generation: a piece of text, a lookup the
// model asked for, or the end.
type draftEvent struct {
	seq  int
	text string
	// reset says the text so far was a lead-in to a lookup, not the draft.
	reset bool
	// note is what the model is doing, for the status line; noteSet tells
	// an empty note from no note.
	note    string
	noteSet bool
	done    bool
	err     error
}

// assistState is a generation in flight.
type assistState struct {
	seq    int
	cancel context.CancelFunc
	events <-chan draftEvent
	// before is the body as it was, put back when the generation is stopped
	// or fails: a draft that did not arrive must not have taken anything.
	before string
	// quoted is the quote below the person's own text, kept out of the
	// model's way and put back under its draft.
	quoted string
	text   strings.Builder
	// note is the lookup under way, shown on the status line.
	note string
}

var errNoAI = errors.New("no AI model configured — add an [[ai.models]] block to config.toml")

// startAsk is ctrl+g: it opens the instructions prompt. When there is text
// above the quote it asks first, because the draft replaces that text -- it
// is carried into the prompt as what the person meant, but it is replaced.
func (c *composeView) startAsk() tea.Cmd {
	if c.d.AI == nil {
		c.err = errNoAI
		return nil
	}
	own, _ := c.split()
	if strings.TrimSpace(own) != "" && c.pending != pendingReplace {
		c.pending = pendingReplace
		return nil
	}
	c.pending, c.err, c.info = pendingNone, nil, ""
	c.asking, c.instr = true, ""
	return nil
}

// askKey is a key while the instructions prompt is open, the same idiom as
// the list's search prompt.
func (c *composeView) askKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		c.asking = false
		return c.startAssist(strings.TrimSpace(c.instr))
	case "esc":
		c.asking, c.instr = false, ""
		return nil
	case "backspace":
		if r := []rune(c.instr); len(r) > 0 {
			c.instr = string(r[:len(r)-1])
		}
		return nil
	case "ctrl+u":
		c.instr = ""
		return nil
	}
	if s := msg.String(); len([]rune(s)) == 1 {
		c.instr += s
	} else if msg.Text != "" {
		c.instr += msg.Text // a paste, or a character that arrived as text
	}
	return nil
}

// split divides the body into the person's own text and the quote under it.
// A reply knows the quote it opened with and looks for exactly that; a stored
// draft, or a reply whose quote has been trimmed, falls back to the same
// heuristics the reader uses to find where quoting starts.
func (c *composeView) split() (own, quoted string) {
	body := c.body.Value()
	if c.quote != "" {
		if i := strings.Index(body, c.quote); i >= 0 {
			return body[:i], body[i:]
		}
	}
	return mime.SplitQuote(body)
}

// startAssist begins a generation with the instructions given.
func (c *composeView) startAssist(instructions string) tea.Cmd {
	own, quoted := c.split()
	ctx, cancel := context.WithCancel(context.Background())
	c.assistSeq++
	ch := make(chan draftEvent, 64)
	c.assist = &assistState{
		seq:    c.assistSeq,
		cancel: cancel,
		events: ch,
		before: c.body.Value(),
		quoted: quoted,
	}
	c.err, c.info, c.pending = nil, "", pendingNone
	// The body shows the draft arriving, and only that: the quote goes back
	// under it once the draft is whole.
	c.body.SetValue("")
	in := ai.ReplyInput{
		Self:         c.from,
		Answering:    c.orig,
		Instructions: instructions,
		Written:      own,
		Loc:          c.d.loc(),
	}
	return tea.Batch(c.d.draftReply(ctx, c.assist.seq, c.account, c.threadID, in, ch), nextDraft(ch))
}

// stopAssist is esc while a draft is arriving: the generation is cancelled
// and the body goes back to what it was.
func (c *composeView) stopAssist() {
	a := c.assist
	c.assist = nil
	a.cancel()
	c.body.SetValue(a.before)
	c.body.MoveToBegin()
	c.info = "draft stopped — the text is as it was"
}

// onDraft folds one event into the body.
func (c *composeView) onDraft(ev draftEvent) tea.Cmd {
	a := c.assist
	if a == nil || ev.seq != a.seq {
		return nil // a generation that was stopped, still winding down
	}
	if ev.err != nil {
		c.assist = nil
		a.cancel()
		c.body.SetValue(a.before)
		c.body.MoveToBegin()
		switch {
		case errors.Is(ev.err, context.DeadlineExceeded):
			c.err = errors.New("the model took too long — raise its timeout in config.toml, or try a smaller model")
		case errors.Is(ev.err, context.Canceled):
			c.info = "draft stopped — the text is as it was"
		default:
			c.err = ev.err
		}
		return nil
	}
	if !ev.done {
		if ev.reset {
			a.text.Reset()
			c.body.SetValue("")
		}
		if ev.noteSet {
			a.note = ev.note
		}
		if ev.text != "" {
			a.text.WriteString(ev.text)
			c.body.InsertString(ev.text)
		}
		return nextDraft(a.events)
	}

	c.assist = nil
	a.cancel()
	text := ai.CleanText(a.text.String())
	if text == "" {
		c.body.SetValue(a.before)
		c.body.MoveToBegin()
		c.err = errors.New("the model returned nothing")
		return nil
	}
	body := text
	if q := strings.TrimLeft(a.quoted, "\n"); q != "" {
		body = strings.TrimRight(text, "\n") + "\n\n" + q
	}
	c.body.SetValue(body)
	c.body.MoveToBegin()
	c.info = "drafted by " + c.d.AI.Describe() + " — read it before it goes"
	return nil
}

// assistFooter is the status line while a draft is arriving.
func (c *composeView) assistFooter() string {
	s := "drafting with " + c.d.AI.Describe() + "…"
	if c.assist != nil && c.assist.note != "" {
		s += " " + c.assist.note
	}
	return s + " · esc to stop"
}

// lookupNote says what a lookup is, in a few words: "mail search offerte
// from=anna".
func lookupNote(call ai.ToolCall) string {
	var args map[string]any
	_ = json.Unmarshal(call.Arguments, &args)
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{strings.ReplaceAll(call.Name, "_", " ")}
	for _, k := range keys {
		v := fmt.Sprint(args[k])
		if f, ok := args[k].(float64); ok {
			v = strconv.FormatFloat(f, 'f', -1, 64)
		}
		if k == "query" || k == "id" {
			parts = append(parts, `"`+v+`"`)
		} else {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

// nextDraft waits for the next event of a generation. It returns nil once
// the channel is closed, which ends the chain.
func nextDraft(ch <-chan draftEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return ev
	}
}

// draftReply is the Cmd that runs a generation: it reads the thread, asks the
// model its window, builds the prompt and streams the model's answer into ch
// as draftEvents, then a final one saying it is done or why not, and closes
// ch.
//
// The thread comes off disk here rather than being carried by the composer,
// which only ever held the one message it opened on; a stored draft being
// finished holds none at all, only the thread id. When the thread cannot be
// read the message in hand is what the model gets, which is still a reply.
func (d Deps) draftReply(ctx context.Context, seq int, account, thread string, in ai.ReplyInput, ch chan<- draftEvent) tea.Cmd {
	return func() tea.Msg {
		defer close(ch)
		send := func(ev draftEvent) {
			ev.seq = seq
			select {
			case ch <- ev:
			case <-ctx.Done():
			}
		}
		if d.Store != nil && thread != "" {
			lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			_, msgs, err := d.Store.GetThread(lctx, account, thread, false)
			cancel()
			if err != nil {
				d.log().Warn("ai draft: read thread", "account", account, "thread", thread, "err", err)
			} else {
				in.Thread = msgs
			}
		}
		in.ContextWindow = d.AI.ContextWindow() // may go to the server; that is why it is here
		in.Lookups = d.Tools != nil
		req := ai.ReplyPrompt(in)
		_, err := ai.Run(ctx, d.AI, req, d.Tools, ai.Observer{
			Text:    func(s string) { send(draftEvent{text: s}) },
			Discard: func() { send(draftEvent{reset: true}) },
			Lookup: func(call ai.ToolCall) {
				d.log().Info("ai draft: lookup", "tool", call.Name, "args", string(call.Arguments))
				send(draftEvent{note: "· " + lookupNote(call), noteSet: true})
			},
			Result: func(call ai.ToolCall, out string, err error) {
				if err != nil {
					d.log().Warn("ai draft: lookup failed", "tool", call.Name, "err", err)
				}
				send(draftEvent{note: "· " + lookupNote(call) + " ✓ · thinking", noteSet: true})
			},
		})
		send(draftEvent{done: true, err: err})
		return nil
	}
}
