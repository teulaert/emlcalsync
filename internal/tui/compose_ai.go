package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/ai"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
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
// (esc) and its output arrives piecemeal, each piece a modelEvent read off a
// channel by the next Cmd in the chain. The events carry the seq of the
// generation they belong to, so a stopped generation's stragglers are
// ignored, the same way stale loads are everywhere else.

// modelEvent is one step of a generation: a piece of text, a lookup the
// model asked for, or the end. The composer's draft and the summary screen
// read the same events off the same kind of channel.
type modelEvent struct {
	seq  int
	text string
	// final is the whole answer, on the done event: what Run returned, which
	// is what the text pieces added up to unless a lookup reset them.
	final string
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
	events <-chan modelEvent
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
	ch := make(chan modelEvent, 64)
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
	return tea.Batch(c.d.draftReply(ctx, c.assist.seq, c.account, c.threadID, in, ch), nextEvent(ch))
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
func (c *composeView) onDraft(ev modelEvent) tea.Cmd {
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
		return nextEvent(a.events)
	}

	c.assist = nil
	a.cancel()
	text := ai.CleanText(ev.final)
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

// nextEvent waits for the next event of a generation. It returns nil once
// the channel is closed, which ends the chain.
func nextEvent(ch <-chan modelEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return ev
	}
}

// draftReply runs the reply draft: the thread comes off disk, the prompt is
// built round it and the model's answer streams back as modelEvents.
//
// The thread comes off disk here rather than being carried by the composer,
// which only ever held the one message it opened on; a stored draft being
// finished holds none at all, only the thread id. When the thread cannot be
// read the message in hand is what the model gets, which is still a reply.
func (d Deps) draftReply(ctx context.Context, seq int, account, thread string, in ai.ReplyInput, ch chan<- modelEvent) tea.Cmd {
	return d.runModel(ctx, seq, modelJob{
		what: "draft", account: account, thread: thread,
		build: func(msgs []model.Message, window int, lookups bool) ai.Request {
			if len(msgs) > 0 {
				in.Thread = msgs
			}
			in.ContextWindow, in.Lookups = window, lookups
			return ai.ReplyPrompt(in)
		},
	}, ch)
}

// modelJob is one thing to ask the model about a conversation.
type modelJob struct {
	what            string // for the log: "draft", "summary"
	account, thread string
	// build makes the request out of the thread as read off disk (empty when
	// it could not be), the model's window and whether it has tools.
	build func(msgs []model.Message, window int, lookups bool) ai.Request
	// key and cache, when set, remember the answer for the thread as it is
	// now: the same question about the same messages is not asked twice.
	key   func(msgs []model.Message) string
	cache *answerCache
}

// runModel is the Cmd behind every generation: it reads the thread, asks the
// model its window, builds the request and streams the answer into ch as
// modelEvents -- text, lookups, resets -- then a final one saying it is done
// or why not, and closes ch. Everything blocking happens here, off the
// update loop; the screen only ever reads events.
func (d Deps) runModel(ctx context.Context, seq int, job modelJob, ch chan<- modelEvent) tea.Cmd {
	return func() tea.Msg {
		defer close(ch)
		send := func(ev modelEvent) {
			ev.seq = seq
			select {
			case ch <- ev:
			case <-ctx.Done():
			}
		}
		var msgs []model.Message
		if d.Store != nil && job.thread != "" {
			lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			_, got, err := d.Store.GetThread(lctx, job.account, job.thread, false)
			cancel()
			if err != nil {
				d.log().Warn("ai "+job.what+": read thread", "account", job.account, "thread", job.thread, "err", err)
			} else {
				msgs = got
			}
		}
		var key string
		if job.key != nil && job.cache != nil {
			key = job.key(msgs)
			if out, ok := job.cache.get(key); ok {
				send(modelEvent{text: out})
				send(modelEvent{done: true, final: out})
				return nil
			}
		}
		window := d.AI.ContextWindow() // may go to the server; that is why it is here
		req := job.build(msgs, window, d.Tools != nil)
		out, err := ai.Run(ctx, d.AI, req, d.Tools, ai.Observer{
			Text:    func(s string) { send(modelEvent{text: s}) },
			Discard: func() { send(modelEvent{reset: true}) },
			Lookup: func(call ai.ToolCall) {
				d.log().Info("ai "+job.what+": lookup", "tool", call.Name, "args", string(call.Arguments))
				send(modelEvent{note: "· " + lookupNote(call), noteSet: true})
			},
			Result: func(call ai.ToolCall, out string, err error) {
				if err != nil {
					d.log().Warn("ai "+job.what+": lookup failed", "tool", call.Name, "err", err)
				}
				send(modelEvent{note: "· " + lookupNote(call) + " ✓ · thinking", noteSet: true})
			},
		})
		if err == nil && key != "" {
			job.cache.put(key, out)
		}
		send(modelEvent{done: true, final: out, err: err})
		return nil
	}
}

// answerCache remembers what the model said about a conversation as it was,
// so going back and forth between the list and a summary does not ask
// again. It is written from Cmd goroutines, hence the lock, and it is
// bounded: a session is not that long.
type answerCache struct {
	mu sync.Mutex
	m  map[string]string
}

const answerCacheMax = 200

func newAnswerCache() *answerCache { return &answerCache{m: map[string]string{}} }

func (c *answerCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, ok
}

func (c *answerCache) put(key, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= answerCacheMax {
		c.m = map[string]string{}
	}
	c.m[key] = v
}
