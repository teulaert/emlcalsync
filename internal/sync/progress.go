package sync

import (
	"fmt"
	"strconv"
	"strings"
	stdsync "sync"
	"time"
)

// Live progress.
//
// A backfill of half a million messages spends minutes inside one Enumerate
// page, so a counter that only moves per 100-message index transaction looks
// frozen. A meter is fed from the FetchRaw callback instead and emits a
// ProgressEvent every progressEvery messages (or progressInterval, whichever
// comes first) carrying the running count, the total when the provider gave
// us one, and a rate + ETA in Message:
//
//	1 234/52 000 · 48/s · ~18m
const (
	// progressEvery is how many messages go by between progress events.
	progressEvery = 25
	// progressInterval keeps the line alive when messages arrive slowly.
	progressInterval = 250 * time.Millisecond
)

// meter turns a running count into throttled ProgressEvents.
type meter struct {
	e        *Engine
	account  string
	resource string

	mu        stdsync.Mutex
	phase     string
	done      int
	total     int
	startedAt time.Time
	startDone int
	lastAt    time.Time
	lastDone  int
}

// newMeter starts a meter at done out of total (total 0 = unknown). done is
// where a resumed backfill picks up, so the rate is measured from this run
// only while the counter keeps counting the whole job.
func (e *Engine) newMeter(account, resource, phase string, done, total int) *meter {
	now := time.Now()
	return &meter{
		e: e, account: account, resource: resource, phase: phase,
		done: done, total: total,
		startedAt: now, startDone: done, lastAt: now, lastDone: done,
	}
}

func (m *meter) setTotal(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.mu.Lock()
	m.total = n
	m.mu.Unlock()
}

// add advances the counter by n and emits an event when enough has happened.
func (m *meter) add(n int) {
	if m == nil || n == 0 {
		return
	}
	m.mu.Lock()
	m.done += n
	ev, ok := m.eventLocked(false, "")
	m.mu.Unlock()
	if ok {
		m.e.emit(ev)
	}
}

// set moves the counter to an exact value and always emits: it is what a page
// boundary knows and the per-message counter only approximates.
func (m *meter) set(n int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.done = n
	ev, _ := m.eventLocked(true, "")
	m.mu.Unlock()
	m.e.emit(ev)
}

// mark moves the counter to n and emits it with an extra word
// ("enumerated", "refreshed", "applied").
func (m *meter) mark(n int, note string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.done = n
	ev, _ := m.eventLocked(true, note)
	m.mu.Unlock()
	m.e.emit(ev)
}

func (m *meter) eventLocked(force bool, note string) (ProgressEvent, bool) {
	now := time.Now()
	if !force && m.done-m.lastDone < progressEvery && now.Sub(m.lastAt) < progressInterval {
		return ProgressEvent{}, false
	}
	m.lastAt, m.lastDone = now, m.done
	msg := m.messageLocked(now)
	if note != "" {
		msg += " · " + note
	}
	return ProgressEvent{
		Account:  m.account,
		Resource: m.resource,
		Phase:    m.phase,
		Done:     m.done,
		Total:    m.total,
		Message:  msg,
	}, true
}

// messageLocked renders "1 234/52 000 · 48/s · ~18m", dropping the parts it
// cannot know yet.
func (m *meter) messageLocked(now time.Time) string {
	parts := []string{humanCount(m.done)}
	if m.total > 0 {
		parts[0] += "/" + humanCount(m.total)
	}
	if rate := m.rateLocked(now); rate > 0 {
		parts = append(parts, fmt.Sprintf("%.0f/s", rate))
		if m.total > m.done {
			eta := time.Duration(float64(m.total-m.done) / rate * float64(time.Second))
			parts = append(parts, "~"+shortDur(eta))
		}
	}
	return strings.Join(parts, " · ")
}

// rateLocked is messages per second over this run. It stays 0 until a batch's
// worth of messages has gone by, because a rate extrapolated from three of
// them is noise, and an ETA built on it is worse than none.
func (m *meter) rateLocked(now time.Time) float64 {
	elapsed := now.Sub(m.startedAt)
	if elapsed <= 0 || m.done-m.startDone < progressEvery {
		return 0
	}
	return float64(m.done-m.startDone) / elapsed.Seconds()
}

// humanCount renders a count as plain digits. No thousands separator: a
// space reads as two numbers on a terminal ("66 808" looked like 66 and 808).
func humanCount(n int) string { return strconv.Itoa(n) }
