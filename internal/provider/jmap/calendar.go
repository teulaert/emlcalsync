package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/provider"
)

// eventPageMax bounds one CalendarEvent/query page during a full listing.
const eventPageMax = 500

// Calendar implements provider.CalendarProvider against a JMAP calendars
// account.
//
// CAVEAT: JMAP for Calendars is still an IETF draft
// (draft-ietf-jmap-calendars). Fastmail ships an implementation but its public
// documentation still lists calendars as CalDAV-only, so the exact spelling of
// a few things may differ from the draft this was written against (-28).
// Everywhere that matters this file probes and falls back rather than assuming:
//
//   - the query filter is sent as {"inCalendar": id} (draft -20 and later) and
//     retried as {"inCalendars": [id]} (draft -08 and earlier) if the server
//     rejects it;
//   - "sendSchedulingMessages" is retried without the argument if the server
//     does not know it;
//   - Calendar/get requests all properties rather than naming them, so an
//     unknown property name cannot fail the call.
//
// See the summary at the end of this package's tests for what to verify
// against a live account first.
type Calendar struct {
	c *Client

	mu           sync.Mutex
	accountID    string
	capURN       string // capability URN calendars live under on this server
	filterPlural bool   // server wants the legacy {"inCalendars": [...]} filter
	filterProbed bool
	noScheduling bool // server rejected sendSchedulingMessages
}

// Calendar returns the calendar provider bound to the token's primary
// calendars account.
func (c *Client) Calendar() *Calendar { return &Calendar{c: c} }

var _ provider.CalendarProvider = (*Calendar)(nil)

// AccountID resolves (and caches) the primary account id for calendars, along
// with the capability URN this server publishes them under.
//
// Every calendar method calls this before it builds a request, so using() can
// rely on the URN being resolved by the time it is consulted.
func (cal *Calendar) AccountID(ctx context.Context) (string, error) {
	cal.mu.Lock()
	id := cal.accountID
	cal.mu.Unlock()
	if id != "" {
		return id, nil
	}
	urn, id, err := cal.c.CalendarCapability(ctx)
	if err != nil {
		return "", err
	}
	cal.mu.Lock()
	cal.accountID, cal.capURN = id, urn
	cal.mu.Unlock()
	return id, nil
}

// using returns the capability set for a calendar request: whichever URN this
// server publishes calendars under, defaulting to the standard one until
// AccountID has resolved it.
func (cal *Calendar) using() []string {
	cal.mu.Lock()
	urn := cal.capURN
	cal.mu.Unlock()
	if urn == "" {
		urn = CapCalendars
	}
	return []string{urn}
}

// ---------------------------------------------------------------------------
// Calendars

type calendarRights struct {
	MayReadFreeBusy  bool `json:"mayReadFreeBusy"`
	MayReadItems     bool `json:"mayReadItems"`
	MayWriteAll      bool `json:"mayWriteAll"`
	MayWriteOwn      bool `json:"mayWriteOwn"`
	MayUpdatePrivate bool `json:"mayUpdatePrivate"`
	MayRSVP          bool `json:"mayRSVP"`
	MayShare         bool `json:"mayShare"`
	MayDelete        bool `json:"mayDelete"`
	// Older drafts spelled the write rights differently; accept both.
	MayAddItems  *bool `json:"mayAddItems"`
	MayModifyAll *bool `json:"mayModifyAll"`
	MayAdmin     *bool `json:"mayAdmin"`
}

func (r *calendarRights) accessRole() string {
	if r == nil {
		// No rights object (or a server that does not send one): this is the
		// user's own account, so assume full control.
		return "owner"
	}
	if r.MayShare || derefBool(r.MayAdmin) {
		return "owner"
	}
	if r.MayWriteAll || r.MayWriteOwn || derefBool(r.MayModifyAll) || derefBool(r.MayAddItems) {
		return "writer"
	}
	if r.MayReadItems {
		return "reader"
	}
	return ""
}

func derefBool(b *bool) bool { return b != nil && *b }

type calendarObject struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Color     string          `json:"color"`
	SortOrder int             `json:"sortOrder"`
	IsDefault bool            `json:"isDefault"`
	IsVisible *bool           `json:"isVisible"`
	TimeZone  *string         `json:"timeZone"`
	MyRights  *calendarRights `json:"myRights"`
}

type calendarGetResponse struct {
	AccountID string           `json:"accountId"`
	State     string           `json:"state"`
	List      []calendarObject `json:"list"`
	NotFound  []string         `json:"notFound"`
}

// Calendars returns every calendar in the account.
func (cal *Calendar) Calendars(ctx context.Context) ([]model.Calendar, error) {
	acct, err := cal.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	var got calendarGetResponse
	// No "properties" argument on purpose: naming a property the server does
	// not implement would fail the whole call, and calendar lists are tiny.
	if err := cal.c.call(ctx, cal.using(), "Calendar/get",
		map[string]any{"accountId": acct, "ids": nil}, &got); err != nil {
		return nil, err
	}
	out := make([]model.Calendar, 0, len(got.List))
	for _, c := range got.List {
		mc := model.Calendar{
			RemoteID:   c.ID,
			Name:       c.Name,
			Color:      c.Color,
			Primary:    c.IsDefault,
			AccessRole: c.MyRights.accessRole(),
		}
		if c.TimeZone != nil {
			mc.Timezone = *c.TimeZone
		}
		out = append(out, mc)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Events

type eventGetResponse struct {
	AccountID string            `json:"accountId"`
	State     string            `json:"state"`
	List      []json.RawMessage `json:"list"`
	NotFound  []string          `json:"notFound"`
}

// decodeEvents parses the raw list, keeping each object's bytes for RawJSON.
func decodeEvents(list []json.RawMessage) ([]*jsEvent, []json.RawMessage, error) {
	evs := make([]*jsEvent, 0, len(list))
	raws := make([]json.RawMessage, 0, len(list))
	for _, raw := range list {
		var js jsEvent
		if err := json.Unmarshal(raw, &js); err != nil {
			return nil, nil, fmt.Errorf("jmap: decoding CalendarEvent: %w", err)
		}
		evs = append(evs, &js)
		raws = append(raws, raw)
	}
	return evs, raws, nil
}

// inCalendarFilter builds the query filter, using whichever spelling this
// server accepts.
func (cal *Calendar) inCalendarFilter(calendarRemote string) map[string]any {
	cal.mu.Lock()
	plural := cal.filterPlural
	cal.mu.Unlock()
	if plural {
		return map[string]any{"inCalendars": []string{calendarRemote}}
	}
	return map[string]any{"inCalendar": calendarRemote}
}

// isFilterRejection reports whether an error looks like "I do not know that
// filter condition".
func isFilterRejection(err error) bool {
	return IsMethodError(err, "unsupportedFilter") ||
		IsMethodError(err, "invalidArguments") ||
		IsMethodError(err, "invalidFilter")
}

// queryEventIDs lists every event id in one calendar, paging until exhausted.
func (cal *Calendar) queryEventIDs(ctx context.Context, acct, calendarRemote string) ([]string, error) {
	var ids []string
	for position := 0; ; {
		args := map[string]any{
			"accountId": acct,
			"filter":    cal.inCalendarFilter(calendarRemote),
			"position":  position,
			"limit":     eventPageMax,
		}
		var q queryResponse
		err := cal.c.call(ctx, cal.using(), "CalendarEvent/query", args, &q)
		if err != nil {
			cal.mu.Lock()
			probed, plural := cal.filterProbed, cal.filterPlural
			cal.mu.Unlock()
			if isFilterRejection(err) && !probed && !plural {
				// Fall back to the pre-draft-20 plural spelling and retry once.
				cal.c.log.Debug("jmap: CalendarEvent/query rejected inCalendar, trying inCalendars")
				cal.mu.Lock()
				cal.filterPlural, cal.filterProbed = true, true
				cal.mu.Unlock()
				continue
			}
			return nil, err
		}
		cal.mu.Lock()
		cal.filterProbed = true
		cal.mu.Unlock()

		ids = append(ids, q.IDs...)
		limit := eventPageMax
		if q.Limit != nil && *q.Limit > 0 {
			limit = *q.Limit
		}
		if len(q.IDs) == 0 || len(q.IDs) < limit {
			return ids, nil
		}
		position += len(q.IDs)
	}
}

// getEvents fetches events by id in chunks, honouring maxObjectsInGet.
func (cal *Calendar) getEvents(ctx context.Context, acct string, ids []string) ([]*jsEvent, []json.RawMessage, string, error) {
	s, err := cal.c.Session(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	var (
		evs   []*jsEvent
		raws  []json.RawMessage
		state string
	)
	for chunk := range slices.Chunk(ids, max(1, s.Core.MaxObjectsInGet)) {
		var got eventGetResponse
		if err := cal.c.call(ctx, cal.using(), "CalendarEvent/get", map[string]any{
			"accountId": acct,
			"ids":       chunk,
		}, &got); err != nil {
			return nil, nil, "", err
		}
		e, r, err := decodeEvents(got.List)
		if err != nil {
			return nil, nil, "", err
		}
		evs = append(evs, e...)
		raws = append(raws, r...)
		state = got.State
	}
	return evs, raws, state, nil
}

// eventState reads the account-wide CalendarEvent state without fetching data.
func (cal *Calendar) eventState(ctx context.Context, acct string) (string, error) {
	var got eventGetResponse
	if err := cal.c.call(ctx, cal.using(), "CalendarEvent/get",
		map[string]any{"accountId": acct, "ids": []string{}}, &got); err != nil {
		return "", err
	}
	return got.State, nil
}

// EventChanges returns changes to one calendar.
//
// since=="" performs a full listing. The state token is read *before* the
// listing so a long backfill replays rather than skips concurrent edits.
//
// The CalendarEvent state is per account, not per calendar, so every calendar
// in an account is handed the same NewState. That is harmless: an event that
// changed in another calendar is simply filtered out here.
func (cal *Calendar) EventChanges(ctx context.Context, calendarRemote, since string) (*provider.EventChanges, error) {
	acct, err := cal.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	if calendarRemote == "" {
		return nil, errors.New("jmap: EventChanges needs a calendar id")
	}
	self := cal.c.AccountEmail()

	if since == "" {
		state, err := cal.eventState(ctx, acct)
		if err != nil {
			return nil, err
		}
		ids, err := cal.queryEventIDs(ctx, acct, calendarRemote)
		if err != nil {
			return nil, err
		}
		out := &provider.EventChanges{NewState: state}
		if len(ids) == 0 {
			return out, nil
		}
		evs, raws, _, err := cal.getEvents(ctx, acct, ids)
		if err != nil {
			return nil, err
		}
		for i, js := range evs {
			out.Upserted = append(out.Upserted,
				toModelEvent(js, raws[i], calendarRemote, self, cal.c.log))
		}
		return out, nil
	}

	created, updated, destroyed, newState, err := cal.c.collectChanges(
		ctx, cal.using(), "CalendarEvent/changes", acct, since)
	if err != nil {
		if IsMethodError(err, "cannotCalculateChanges") {
			return nil, provider.ErrStateExpired
		}
		return nil, err
	}

	out := &provider.EventChanges{NewState: newState, Removed: destroyed}

	gone := make(map[string]bool, len(destroyed))
	for _, id := range destroyed {
		gone[id] = true
	}
	wasUpdated := make(map[string]bool, len(updated))
	seen := make(map[string]bool, len(created)+len(updated))
	want := make([]string, 0, len(created)+len(updated))
	for _, id := range slices.Concat(created, updated) {
		if gone[id] || seen[id] {
			continue
		}
		seen[id] = true
		want = append(want, id)
	}
	for _, id := range updated {
		wasUpdated[id] = true
	}
	if len(want) == 0 {
		return out, nil
	}

	evs, raws, _, err := cal.getEvents(ctx, acct, want)
	if err != nil {
		return nil, err
	}
	for i, js := range evs {
		if !js.CalendarIDs[calendarRemote] {
			// Not (or no longer) in this calendar. If it changed rather than
			// appeared, it may have been moved out from under us.
			if wasUpdated[js.ID] {
				out.Removed = append(out.Removed, js.ID)
			}
			continue
		}
		out.Upserted = append(out.Upserted,
			toModelEvent(js, raws[i], calendarRemote, self, cal.c.log))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Event writes

// setEvents issues CalendarEvent/set, retrying once without
// sendSchedulingMessages if the server does not recognise that argument.
func (cal *Calendar) setEvents(ctx context.Context, args map[string]any, scheduling bool) (*setResponse, error) {
	cal.mu.Lock()
	noSched := cal.noScheduling
	cal.mu.Unlock()

	send := scheduling && !noSched
	for {
		call := make(map[string]any, len(args)+1)
		for k, v := range args {
			call[k] = v
		}
		if send {
			call["sendSchedulingMessages"] = true
		}
		var sr setResponse
		err := cal.c.call(ctx, cal.using(), "CalendarEvent/set", call, &sr)
		if err != nil {
			if send && IsMethodError(err, "invalidArguments") {
				cal.c.log.Debug("jmap: server rejected sendSchedulingMessages, retrying without it")
				cal.mu.Lock()
				cal.noScheduling = true
				cal.mu.Unlock()
				send = false
				continue
			}
			return nil, err
		}
		return &sr, nil
	}
}

// getEvent fetches one event, or model.ErrNotFound.
func (cal *Calendar) getEvent(ctx context.Context, acct, id string) (*jsEvent, json.RawMessage, error) {
	var got eventGetResponse
	if err := cal.c.call(ctx, cal.using(), "CalendarEvent/get", map[string]any{
		"accountId": acct,
		"ids":       []string{id},
	}, &got); err != nil {
		return nil, nil, err
	}
	evs, raws, err := decodeEvents(got.List)
	if err != nil {
		return nil, nil, err
	}
	if len(evs) == 0 {
		return nil, nil, fmt.Errorf("jmap: event %s: %w", id, model.ErrNotFound)
	}
	return evs[0], raws[0], nil
}

// CreateEvent adds an event to a calendar and returns it as the server stored it.
func (cal *Calendar) CreateEvent(ctx context.Context, calendarRemote string, ev *model.Event) (*model.Event, error) {
	acct, err := cal.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	js, err := fromModelEvent(ev, calendarRemote, nil, cal.c.log)
	if err != nil {
		return nil, err
	}
	const cid = "new"
	sr, err := cal.setEvents(ctx, map[string]any{
		"accountId": acct,
		"create":    map[string]any{cid: js},
	}, len(ev.Attendees) > 0)
	if err != nil {
		return nil, err
	}
	if se, bad := sr.NotCreated[cid]; bad {
		return nil, fmt.Errorf("jmap: creating event: %w", se)
	}
	var created struct {
		ID string `json:"id"`
	}
	raw, ok := sr.Created[cid]
	if !ok {
		return nil, errors.New("jmap: CalendarEvent/set created nothing")
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" {
		return nil, errors.New("jmap: CalendarEvent/set returned no event id")
	}
	return cal.fetchModelEvent(ctx, acct, calendarRemote, created.ID)
}

func (cal *Calendar) fetchModelEvent(ctx context.Context, acct, calendarRemote, id string) (*model.Event, error) {
	js, raw, err := cal.getEvent(ctx, acct, id)
	if err != nil {
		return nil, err
	}
	out := toModelEvent(js, raw, calendarRemote, cal.c.AccountEmail(), cal.c.log)
	return &out, nil
}

// managedEventKeys are the JSCalendar properties this package owns. An update
// patches only these, so alerts, colours, per-occurrence overrides and any
// vendor extension on the server survive a write.
var managedEventKeys = []string{
	"title", "description", "start", "duration", "timeZone", "showWithoutTime",
	"status", "locations", "recurrenceRules", "participants", "replyTo", "calendarIds",
}

// UpdateEvent writes the changed top-level properties of ev.
func (cal *Calendar) UpdateEvent(ctx context.Context, ev *model.Event) (*model.Event, error) {
	acct, err := cal.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	if ev.RemoteID == "" {
		return nil, errors.New("jmap: UpdateEvent needs a remote id")
	}

	var base *jsEvent
	baseMap := map[string]any{}
	if len(ev.RawJSON) > 0 {
		var b jsEvent
		if err := json.Unmarshal(ev.RawJSON, &b); err == nil {
			base = &b
			_ = json.Unmarshal(ev.RawJSON, &baseMap)
		}
	}
	if base == nil {
		// No cached server object: re-read it so we still patch minimally.
		b, raw, err := cal.getEvent(ctx, acct, ev.RemoteID)
		if err != nil {
			return nil, err
		}
		base = b
		_ = json.Unmarshal(raw, &baseMap)
	}

	desired, err := fromModelEvent(ev, ev.CalendarRemote, base, cal.c.log)
	if err != nil {
		return nil, err
	}
	desiredMap := map[string]any{}
	blob, err := json.Marshal(desired)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(blob, &desiredMap); err != nil {
		return nil, err
	}

	patch := map[string]any{}
	for _, k := range managedEventKeys {
		want, haveWant := desiredMap[k]
		got, haveGot := baseMap[k]
		switch {
		case haveWant && (!haveGot || !reflect.DeepEqual(want, got)):
			patch[k] = want
		case !haveWant && haveGot:
			patch[k] = nil // property was cleared
		}
	}
	if len(patch) == 0 {
		return cal.fetchModelEvent(ctx, acct, ev.CalendarRemote, ev.RemoteID)
	}

	sr, err := cal.setEvents(ctx, map[string]any{
		"accountId": acct,
		"update":    map[string]any{ev.RemoteID: patch},
	}, len(ev.Attendees) > 0)
	if err != nil {
		return nil, err
	}
	if se, bad := sr.NotUpdated[ev.RemoteID]; bad {
		return nil, fmt.Errorf("jmap: updating event %s: %w", ev.RemoteID, se)
	}
	return cal.fetchModelEvent(ctx, acct, ev.CalendarRemote, ev.RemoteID)
}

// DeleteEvent removes an event. Deleting an event that is already gone is not
// an error.
func (cal *Calendar) DeleteEvent(ctx context.Context, calendarRemote, remoteID string) error {
	acct, err := cal.AccountID(ctx)
	if err != nil {
		return err
	}
	sr, err := cal.setEvents(ctx, map[string]any{
		"accountId": acct,
		"destroy":   []string{remoteID},
	}, true)
	if err != nil {
		return err
	}
	if se, bad := sr.NotDestroyed[remoteID]; bad {
		if se.Type == "notFound" {
			return nil
		}
		return fmt.Errorf("jmap: deleting event %s: %w", remoteID, se)
	}
	return nil
}

// Respond sets the user's own participationStatus on an event.
func (cal *Calendar) Respond(ctx context.Context, calendarRemote, remoteID string, resp model.Participation) error {
	acct, err := cal.AccountID(ctx)
	if err != nil {
		return err
	}
	js, _, err := cal.getEvent(ctx, acct, remoteID)
	if err != nil {
		return err
	}
	self := strings.ToLower(cal.c.AccountEmail())
	if self == "" {
		return errors.New("jmap: cannot RSVP without the account's own email address")
	}
	key := ""
	for _, k := range sortedKeys(js.Participants) {
		if strings.EqualFold(js.Participants[k].address(), self) {
			key = k
			break
		}
	}
	if key == "" {
		return fmt.Errorf("jmap: %s is not a participant of event %s: %w", self, remoteID, model.ErrNotFound)
	}
	patch := map[string]any{
		"participants/" + escapeJSONPointer(key) + "/participationStatus": participationToJS(resp),
	}
	sr, err := cal.setEvents(ctx, map[string]any{
		"accountId": acct,
		"update":    map[string]any{remoteID: patch},
	}, true)
	if err != nil {
		return err
	}
	if se, bad := sr.NotUpdated[remoteID]; bad {
		return fmt.Errorf("jmap: responding to event %s: %w", remoteID, se)
	}
	return nil
}

// escapeJSONPointer escapes a JMAP patch path segment (RFC 6901).
func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}
