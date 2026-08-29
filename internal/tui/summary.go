package tui

import (
	"context"
	"errors"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/ai"
	"github.com/teulaert/emlcalsync/internal/model"
)

// summaryView is ctrl+g on a conversation: what the model has to say about
// it, on a screen of its own. It opens with the prompt on the status line,
// the way the composer's ctrl+g does -- enter alone asks for the summary,
// anything typed is a question about the thread -- and the answer streams
// into a viewport. From here r replies to the conversation, which is the
// point: read four lines, answer, never open the messages.
//
// The answer is remembered per conversation as it stands, so coming back to
// it is free; a new message in the thread is a new conversation.
type summaryView struct {
	d     Deps
	cache *answerCache

	account  string
	threadID string
	subject  string

	// asking is the prompt, open on the status line; instr what is typed.
	asking bool
	instr  string
	// question is what the answer on screen answers; "" is the summary.
	question string

	// run is the generation in flight, nil when none.
	run    *modelRun
	runSeq int

	text string // the finished answer
	err  error
	info string

	vp    viewport.Model
	ready bool
	sized [2]int
}

// modelRun is a generation in flight on the summary screen.
type modelRun struct {
	seq    int
	cancel context.CancelFunc
	events <-chan modelEvent
	note   string
	text   strings.Builder
}

func newSummaryView(d Deps, cache *answerCache, account, thread, subject string) *summaryView {
	s := &summaryView{d: d, cache: cache, account: account, threadID: thread, subject: subject}
	s.asking = true
	return s
}

func (s *summaryView) Title() string {
	subj := strings.TrimSpace(s.subject)
	if subj == "" {
		subj = "(no subject)"
	}
	if s.question != "" && !s.asking && s.run == nil {
		return "answer · " + subj
	}
	return "summary · " + subj
}

func (s *summaryView) Init() tea.Cmd     { return nil }
func (s *summaryView) reload() tea.Cmd   { return nil }
func (s *summaryView) targets() []target { return nil }

// capturingKeys: the prompt takes every key, and so does a running
// generation -- esc is the one thing it answers to.
func (s *summaryView) capturingKeys() bool { return s.asking || s.run != nil }

// ask opens the prompt again over an answer already on screen.
func (s *summaryView) ask() {
	s.asking, s.instr, s.err, s.info = true, "", nil, ""
}

func (s *summaryView) Update(msg tea.Msg, k keymap, w, h int) (screen, tea.Cmd) {
	s.ensure(w, h)
	switch msg := msg.(type) {
	case modelEvent:
		return s, s.onEvent(msg)
	case tea.KeyPressMsg:
		if s.run != nil {
			if msg.String() == "esc" {
				s.stop()
			}
			return s, nil
		}
		if s.asking {
			return s, s.askKey(msg)
		}
		// Scrolling and the like belong to the viewport; the root has
		// already taken r, ctrl+g and esc.
		s.info = ""
		var cmd tea.Cmd
		s.vp, cmd = s.vp.Update(msg)
		return s, cmd
	}
	var cmd tea.Cmd
	s.vp, cmd = s.vp.Update(msg)
	return s, cmd
}

func (s *summaryView) askKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		s.asking = false
		return s.start(strings.TrimSpace(s.instr))
	case "esc":
		s.asking, s.instr = false, ""
		if s.text == "" {
			return closeScreen() // nothing was ever asked: back to where ctrl+g was pressed
		}
		return nil
	case "backspace":
		if r := []rune(s.instr); len(r) > 0 {
			s.instr = string(r[:len(r)-1])
		}
		return nil
	case "ctrl+u":
		s.instr = ""
		return nil
	}
	if t := msg.String(); len([]rune(t)) == 1 {
		s.instr += t
	} else if msg.Text != "" {
		s.instr += msg.Text
	}
	return nil
}

// start asks the model: the summary, or the question given.
func (s *summaryView) start(question string) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	s.runSeq++
	ch := make(chan modelEvent, 64)
	s.run = &modelRun{seq: s.runSeq, cancel: cancel, events: ch}
	s.question, s.err, s.info, s.text = question, nil, "", ""
	s.setContent("")
	self := s.d.sendFrom(s.account)
	job := modelJob{
		what: "summary", account: s.account, thread: s.threadID,
		build: func(msgs []model.Message, window int, lookups bool) ai.Request {
			return ai.SummaryPrompt(ai.SummaryInput{
				Self: s.d.selfFor(s.account, self), Thread: msgs, Question: question,
				ContextWindow: window, Lookups: lookups, Loc: s.d.loc(),
			})
		},
		key:   func(msgs []model.Message) string { return summaryKey(s.account, s.threadID, question, msgs) },
		cache: s.cache,
	}
	return tea.Batch(s.d.runModel(ctx, s.run.seq, job, ch), nextEvent(ch))
}

// summaryKey names a conversation as it stands: the same messages, the same
// question. A new message in the thread changes it.
func summaryKey(account, thread, question string, msgs []model.Message) string {
	newest := ""
	for i := range msgs {
		if !msgs[i].Flags.Draft {
			newest = msgs[i].RemoteID
		}
	}
	return account + "\x00" + thread + "\x00" + newest + "\x00" + strings.ToLower(question)
}

func (s *summaryView) stop() {
	r := s.run
	s.run = nil
	r.cancel()
	s.info = "stopped"
	if s.text == "" {
		s.setContent("")
	}
}

func (s *summaryView) onEvent(ev modelEvent) tea.Cmd {
	r := s.run
	if r == nil || ev.seq != r.seq {
		return nil
	}
	if ev.err != nil {
		s.run = nil
		r.cancel()
		s.setContent(s.text)
		switch {
		case errors.Is(ev.err, context.DeadlineExceeded):
			s.err = errors.New("the model took too long — raise its timeout in config.toml, or try a smaller model")
		case errors.Is(ev.err, context.Canceled):
			s.info = "stopped"
		default:
			s.err = ev.err
		}
		if s.text == "" && s.err != nil {
			s.setContent(s.err.Error())
		}
		return nil
	}
	if !ev.done {
		if ev.reset {
			r.text.Reset()
		}
		if ev.noteSet {
			r.note = ev.note
		}
		if ev.text != "" {
			r.text.WriteString(ev.text)
		}
		s.setContent(r.text.String())
		return nextEvent(r.events)
	}
	s.run = nil
	r.cancel()
	s.text = ai.CleanText(ev.final)
	if s.text == "" {
		s.err = errors.New("the model returned nothing")
		s.setContent(s.err.Error())
		return nil
	}
	s.setContent(s.text)
	return nil
}

func (s *summaryView) ensure(w, h int) {
	if s.sized == [2]int{w, h} || w <= 0 || h <= 0 {
		return
	}
	s.sized = [2]int{w, h}
	vh := max(listRows(h), 1)
	if !s.ready {
		s.vp = viewport.New(viewport.WithWidth(max(w-2, 1)), viewport.WithHeight(vh))
		s.vp.SoftWrap = true
		s.ready = true
		return
	}
	s.vp.SetWidth(max(w-2, 1))
	s.vp.SetHeight(vh)
}

func (s *summaryView) setContent(text string) {
	if !s.ready {
		return
	}
	s.vp.SetContent(text)
	s.vp.GotoTop()
}

func (s *summaryView) View(w, h int) string {
	s.ensure(w, h)
	rows := listRows(h)
	var lines []string
	switch {
	case s.run != nil && s.run.text.Len() == 0:
		lines = []string{" thinking…"}
	case s.asking && s.text == "":
		lines = []string{styleFaint.Render(" ctrl+g · " + s.subject)}
	default:
		for _, l := range strings.Split(s.vp.View(), "\n") {
			lines = append(lines, " "+l)
		}
	}
	for len(lines) < rows {
		lines = append(lines, "")
	}
	return strings.Join(lines[:rows], "\n")
}

func (s *summaryView) footer(w int) string {
	switch {
	case s.run != nil:
		f := "asking " + s.d.AI.Describe() + "…"
		if s.run.note != "" {
			f += " " + s.run.note
		}
		return f + " · esc to stop"
	case s.asking:
		return padCells("ai · ask about this conversation, or enter alone for a summary: "+s.instr+"█", w)
	case s.err != nil:
		return styleErr.Render(s.err.Error())
	case s.info != "":
		return s.info + " · r reply · ctrl+g ask · esc back"
	}
	return "r reply · ctrl+g ask again · esc back"
}

var (
	_ screen    = (*summaryView)(nil)
	_ capturing = (*summaryView)(nil)
)
