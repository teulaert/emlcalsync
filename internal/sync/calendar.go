package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lennert/emlcal/internal/calendar"
	"github.com/lennert/emlcal/internal/config"
	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/provider"
)

// calResource is the sync_state resource name for one calendar.
func calResource(remote string) string { return "cal:" + remote }

// syncCalendar refreshes the account's calendar list and runs a delta (or a
// full listing) for every calendar the config selects.
func (e *Engine) syncCalendar(ctx context.Context, acct config.Account, full bool) (*ResourceReport, error) {
	started := time.Now()
	rep := &ResourceReport{Kind: KindDelta}

	cp, err := e.calendarProvider(ctx, acct)
	if err != nil {
		return rep, err
	}
	cals, err := cp.Calendars(ctx)
	if err != nil {
		return rep, fmt.Errorf("sync: %s: calendars: %w", acct.Name, err)
	}
	wanted := make([]model.Calendar, 0, len(cals))
	for _, c := range cals {
		if acct.WantsCalendar(c.Name) {
			c.AccountID = acct.Name
			wanted = append(wanted, c)
		}
	}
	if err := e.st.ReplaceCalendars(ctx, acct.Name, wanted); err != nil {
		return rep, err
	}

	fullListing := false
	for i := range wanted {
		cal := wanted[i]
		r, listedAll, err := e.syncOneCalendar(ctx, acct, cp, cal, full)
		rep.add(r)
		fullListing = fullListing || listedAll
		if err != nil {
			rep.Duration = time.Since(started)
			e.logSync(ctx, acct.Name, KindCalendar, started, rep, err)
			return rep, err
		}
	}
	if fullListing {
		rep.Kind = KindBackfill
	}
	rep.Duration = time.Since(started)
	if rep.Added+rep.Updated+rep.Removed > 0 {
		e.logSync(ctx, acct.Name, KindCalendar, started, rep, nil)
	}
	return rep, nil
}

// syncOneCalendar applies EventChanges for a single calendar and re-expands
// everything it touched.
func (e *Engine) syncOneCalendar(ctx context.Context, acct config.Account, cp provider.CalendarProvider, cal model.Calendar, full bool) (*ResourceReport, bool, error) {
	rep := &ResourceReport{Kind: KindDelta}
	res := calResource(cal.RemoteID)

	since := ""
	if !full {
		var err error
		since, err = e.st.GetState(ctx, acct.Name, res)
		if err != nil {
			return rep, false, err
		}
	}
	rep.StateBefore = since

	ch, err := cp.EventChanges(ctx, cal.RemoteID, since)
	if errors.Is(err, provider.ErrStateExpired) {
		e.log.Warn("calendar state expired, listing everything",
			"account", acct.Name, "calendar", cal.Name)
		since = ""
		ch, err = cp.EventChanges(ctx, cal.RemoteID, "")
	}
	if err != nil {
		return rep, since == "", fmt.Errorf("sync: %s: calendar %s: %w", acct.Name, cal.Name, err)
	}

	run := &calRun{e: e, acct: acct, cal: cal}
	run.from, run.to = calendar.DefaultWindow(time.Now())

	// Upserts first: an exception can only be applied once its master is in.
	touched := map[string]bool{} // uids whose series needs re-expansion
	var singles []model.Event
	for i := range ch.Upserted {
		ev := ch.Upserted[i]
		ev.AccountID = acct.Name
		if ev.CalendarRemote == "" {
			ev.CalendarRemote = cal.RemoteID
		}
		ev.CalendarID = cal.ID
		if _, err := e.st.UpsertEvent(ctx, &ev); err != nil {
			return rep, since == "", err
		}
		rep.Added++
		switch {
		case ev.RecurrenceID != "" || ev.RRule != "":
			touched[ev.UID] = true
		default:
			singles = append(singles, ev)
		}
	}

	for _, id := range ch.Removed {
		// Look the event up before marking it gone: if it was an exception we
		// have to re-expand its series, which needs the UID.
		if prev, err := e.st.GetEvent(ctx, acct.Name, cal.RemoteID, id); err == nil {
			if prev.UID != "" && (prev.RecurrenceID != "" || prev.RRule != "") {
				touched[prev.UID] = true
			}
		} else if !errors.Is(err, model.ErrNotFound) {
			return rep, since == "", err
		}
		if err := e.st.MarkEventDeleted(ctx, cal.ID, id); err != nil {
			return rep, since == "", err
		}
		rep.Removed++
	}

	for i := range singles {
		if err := run.expandSingle(ctx, &singles[i]); err != nil {
			return rep, since == "", err
		}
	}
	if len(touched) > 0 {
		if err := run.load(ctx); err != nil {
			return rep, since == "", err
		}
		for uid := range touched {
			if err := run.expandSeries(ctx, uid); err != nil {
				return rep, since == "", err
			}
		}
	}

	if ch.NewState != "" {
		if err := e.st.SetState(ctx, acct.Name, res, ch.NewState); err != nil {
			return rep, since == "", err
		}
	}
	rep.StateAfter = ch.NewState
	e.emit(ProgressEvent{
		Account: acct.Name, Resource: "calendar", Phase: rep.Kind,
		Done: rep.Added + rep.Removed, Message: cal.Name,
	})
	return rep, since == "", nil
}

// ---------------------------------------------------------------------------
// Expansion

// calRun caches the rows needed to re-expand a recurring series.
type calRun struct {
	e    *Engine
	acct config.Account
	cal  model.Calendar

	from, to time.Time

	masters    map[string]*model.Event  // uid -> master (has an RRULE)
	exceptions map[string][]model.Event // uid -> exception instances
}

// load reads the calendar's recurring masters and exception instances.
func (c *calRun) load(ctx context.Context) error {
	if c.masters != nil {
		return nil
	}
	c.masters = map[string]*model.Event{}
	c.exceptions = map[string][]model.Event{}

	rec, err := c.e.st.ListRecurringEvents(ctx, []string{c.acct.Name})
	if err != nil {
		return err
	}
	for i := range rec {
		ev := rec[i]
		if ev.CalendarID != c.cal.ID || ev.UID == "" || ev.RecurrenceID != "" {
			continue
		}
		c.masters[ev.UID] = &ev
	}

	ex, err := c.e.st.ListEventExceptions(ctx, c.cal.ID)
	if err != nil {
		return err
	}
	for i := range ex {
		if ex[i].UID == "" {
			continue
		}
		c.exceptions[ex[i].UID] = append(c.exceptions[ex[i].UID], ex[i])
	}
	return nil
}

// expandSingle materialises the one occurrence of a non-recurring event.
func (c *calRun) expandSingle(ctx context.Context, ev *model.Event) error {
	if ev.DeletedAt != nil {
		return c.e.st.ReplaceOccurrences(ctx, ev.ID, nil)
	}
	occ, err := calendar.Expand(ev, c.from, c.to)
	if err != nil {
		c.e.log.Warn("event expansion failed",
			"account", c.acct.Name, "event", ev.RemoteID, "err", err)
		return nil
	}
	return c.e.st.ReplaceOccurrences(ctx, ev.ID, occ)
}

// expandSeries re-expands the master of uid and redistributes the result: the
// generated instances belong to the master, the overridden ones to the
// exception rows that moved them, so the agenda shows each with its own title.
func (c *calRun) expandSeries(ctx context.Context, uid string) error {
	master := c.masters[uid]
	exceptions := c.exceptions[uid]

	if master == nil {
		// An exception without a master (the series lives in another account
		// or was never delivered): treat each instance as a standalone event.
		for i := range exceptions {
			if err := c.expandSingle(ctx, &exceptions[i]); err != nil {
				return err
			}
		}
		return nil
	}

	var occ []model.Occurrence
	if master.DeletedAt == nil {
		var err error
		occ, err = calendar.Expand(master, c.from, c.to)
		if err != nil {
			c.e.log.Warn("series expansion failed",
				"account", c.acct.Name, "event", master.RemoteID, "err", err)
			occ = nil
		}
	}
	occ = calendar.ApplyExceptions(occ, exceptions)

	byEvent := map[int64][]model.Occurrence{}
	for _, o := range occ {
		byEvent[o.EventID] = append(byEvent[o.EventID], o)
	}
	if err := c.e.st.ReplaceOccurrences(ctx, master.ID, byEvent[master.ID]); err != nil {
		return err
	}
	for i := range exceptions {
		ex := &exceptions[i]
		if ex.ID == master.ID {
			continue
		}
		if err := c.e.st.ReplaceOccurrences(ctx, ex.ID, byEvent[ex.ID]); err != nil {
			return err
		}
	}
	return nil
}

// ReexpandAll re-materialises every recurring series against a window centred
// on now. Watch runs it once a day so the agenda keeps reaching two years out.
func (e *Engine) ReexpandAll(ctx context.Context) error {
	for _, acct := range e.cfg.Accounts {
		cals, err := e.st.ListCalendars(ctx, []string{acct.Name})
		if err != nil {
			return err
		}
		for _, cal := range cals {
			run := &calRun{e: e, acct: acct, cal: cal}
			run.from, run.to = calendar.DefaultWindow(time.Now())
			if err := run.load(ctx); err != nil {
				return err
			}
			uids := map[string]bool{}
			for uid := range run.masters {
				uids[uid] = true
			}
			for uid := range run.exceptions {
				uids[uid] = true
			}
			for uid := range uids {
				if err := run.expandSeries(ctx, uid); err != nil {
					return err
				}
			}
			e.emit(ProgressEvent{
				Account: acct.Name, Resource: "calendar", Phase: "reexpand",
				Done: len(uids), Message: cal.Name,
			})
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
	return nil
}
