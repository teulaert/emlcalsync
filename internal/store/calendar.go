package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lennert/emlcal/internal/model"
)

// ---------------------------------------------------------------------------
// Calendars

// ReplaceCalendars makes the stored calendar list for an account match cals:
// rows are upserted by remote id and calendars the provider no longer lists
// are deleted (cascading their events and occurrences).
func (s *Store) ReplaceCalendars(ctx context.Context, accountID string, cals []model.Calendar) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.ReplaceCalendars(ctx, accountID, cals) })
}

func (tx *Tx) ReplaceCalendars(ctx context.Context, accountID string, cals []model.Calendar) error {
	keep := make(map[string]bool, len(cals))
	for i := range cals {
		c := &cals[i]
		var id int64
		err := tx.q.QueryRowContext(ctx, `
			INSERT INTO calendars (account_id, remote_id, name, color, timezone, is_primary, access_role)
			VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(account_id, remote_id) DO UPDATE SET
			  name = excluded.name, color = excluded.color, timezone = excluded.timezone,
			  is_primary = excluded.is_primary, access_role = excluded.access_role
			RETURNING id`,
			accountID, c.RemoteID, c.Name, nullStr(c.Color), nullStr(c.Timezone),
			boolInt(c.Primary), nullStr(c.AccessRole)).Scan(&id)
		if err != nil {
			return fmt.Errorf("store: upsert calendar %s/%s: %w", accountID, c.RemoteID, err)
		}
		c.ID = id
		c.AccountID = accountID
		keep[c.RemoteID] = true
	}

	rows, err := tx.q.QueryContext(ctx, `SELECT remote_id FROM calendars WHERE account_id = ?`, accountID)
	if err != nil {
		return fmt.Errorf("store: list calendars: %w", err)
	}
	var stale []any
	for rows.Next() {
		var remote string
		if err := rows.Scan(&remote); err != nil {
			rows.Close()
			return err
		}
		if !keep[remote] {
			stale = append(stale, remote)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(stale) > 0 {
		args := append([]any{accountID}, stale...)
		if _, err := tx.q.ExecContext(ctx,
			`DELETE FROM calendars WHERE account_id = ? AND remote_id IN (`+placeholders(len(stale))+`)`,
			args...); err != nil {
			return fmt.Errorf("store: delete stale calendars: %w", err)
		}
	}
	return nil
}

const calendarCols = `c.id, c.account_id, c.remote_id, c.name, c.color, c.timezone, c.is_primary, c.access_role`

func scanCalendar(sc scanner) (model.Calendar, error) {
	var c model.Calendar
	var color, tz, role sql.NullString
	var primary int64
	if err := sc.Scan(&c.ID, &c.AccountID, &c.RemoteID, &c.Name, &color, &tz, &primary, &role); err != nil {
		return c, err
	}
	c.Color = color.String
	c.Timezone = tz.String
	c.Primary = primary != 0
	c.AccessRole = role.String
	return c, nil
}

// ListCalendars returns the calendars of the given accounts (all accounts when
// accountIDs is empty), ordered by account then name.
func (s *Store) ListCalendars(ctx context.Context, accountIDs []string) ([]model.Calendar, error) {
	return s.tx().ListCalendars(ctx, accountIDs)
}

func (tx *Tx) ListCalendars(ctx context.Context, accountIDs []string) ([]model.Calendar, error) {
	q := `SELECT ` + calendarCols + ` FROM calendars c`
	var args []any
	if len(accountIDs) > 0 {
		q += ` WHERE c.account_id IN (` + placeholders(len(accountIDs)) + `)`
		args = anySlice(accountIDs)
	}
	q += ` ORDER BY c.account_id, c.is_primary DESC, c.name`
	rows, err := tx.q.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list calendars: %w", err)
	}
	defer rows.Close()
	var out []model.Calendar
	for rows.Next() {
		c, err := scanCalendar(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCalendarByRemote looks a calendar up by provider id.
func (s *Store) GetCalendarByRemote(ctx context.Context, accountID, remote string) (*model.Calendar, error) {
	return s.tx().GetCalendarByRemote(ctx, accountID, remote)
}

func (tx *Tx) GetCalendarByRemote(ctx context.Context, accountID, remote string) (*model.Calendar, error) {
	row := tx.q.QueryRowContext(ctx,
		`SELECT `+calendarCols+` FROM calendars c WHERE c.account_id = ? AND c.remote_id = ?`,
		accountID, remote)
	c, err := scanCalendar(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("calendar %s:%s", accountID, remote)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get calendar %s:%s: %w", accountID, remote, err)
	}
	return &c, nil
}

// FindCalendar resolves a user-supplied calendar: remote id first, then an
// exact case-insensitive name match, then the account's primary calendar when
// nameOrRemote is "" or "primary".
func (s *Store) FindCalendar(ctx context.Context, accountID, nameOrRemote string) (*model.Calendar, error) {
	if nameOrRemote == "" || nameOrRemote == "primary" {
		row := s.db.QueryRowContext(ctx,
			`SELECT `+calendarCols+` FROM calendars c
			  WHERE c.account_id = ? ORDER BY c.is_primary DESC, c.id LIMIT 1`, accountID)
		c, err := scanCalendar(row)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFound("primary calendar for %s", accountID)
		}
		if err != nil {
			return nil, err
		}
		return &c, nil
	}
	if c, err := s.GetCalendarByRemote(ctx, accountID, nameOrRemote); err == nil {
		return c, nil
	} else if !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+calendarCols+` FROM calendars c
		  WHERE c.account_id = ? AND lower(c.name) = ? ORDER BY c.id LIMIT 1`,
		accountID, lowerTrim(nameOrRemote))
	c, err := scanCalendar(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("calendar %q in account %s", nameOrRemote, accountID)
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ---------------------------------------------------------------------------
// Events

const eventCols = `e.id, e.calendar_id, c.account_id, c.remote_id, e.remote_id, e.uid,
	e.title, e.description, e.location, e.start_utc, e.end_utc, e.all_day, e.timezone,
	e.rrule, e.recurrence_id, e.status, e.organizer, e.attendees_json, e.my_response,
	e.raw_json, e.updated_utc, e.deleted_at`

// eventFrom is the FROM clause every event query uses; the calendars join
// supplies AccountID and CalendarRemote.
const eventFrom = ` FROM events e JOIN calendars c ON c.id = e.calendar_id`

func scanEvent(sc scanner) (model.Event, error) {
	var e model.Event
	var uid, title, desc, loc, tz, rrule, recurrenceID, status sql.NullString
	var organizer, attendees, myResp, rawJSON sql.NullString
	var start, end, updated, deletedAt sql.NullInt64
	var allDay int64
	if err := sc.Scan(&e.ID, &e.CalendarID, &e.AccountID, &e.CalendarRemote, &e.RemoteID, &uid,
		&title, &desc, &loc, &start, &end, &allDay, &tz,
		&rrule, &recurrenceID, &status, &organizer, &attendees, &myResp,
		&rawJSON, &updated, &deletedAt); err != nil {
		return e, err
	}
	e.UID = uid.String
	e.Title = title.String
	e.Description = desc.String
	e.Location = loc.String
	e.Start = nullTime(start)
	e.End = nullTime(end)
	e.AllDay = allDay != 0
	e.Timezone = tz.String
	e.RRule = rrule.String
	e.RecurrenceID = recurrenceID.String
	e.Status = model.EventStatus(status.String)
	if organizer.Valid && organizer.String != "" {
		_ = json.Unmarshal([]byte(organizer.String), &e.Organizer)
	}
	if attendees.Valid && attendees.String != "" {
		_ = json.Unmarshal([]byte(attendees.String), &e.Attendees)
	}
	e.MyResponse = model.Participation(myResp.String)
	if rawJSON.Valid && rawJSON.String != "" {
		e.RawJSON = []byte(rawJSON.String)
	}
	e.Updated = nullTime(updated)
	e.DeletedAt = timePtr(deletedAt)
	return e, nil
}

// UpsertEvent inserts or updates one event, resolving CalendarID from
// AccountID + CalendarRemote when it is not set. Returns the local row id.
func (s *Store) UpsertEvent(ctx context.Context, ev *model.Event) (int64, error) {
	var id int64
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		id, err = tx.UpsertEvent(ctx, ev)
		return err
	})
	return id, err
}

func (tx *Tx) UpsertEvent(ctx context.Context, ev *model.Event) (int64, error) {
	calID := ev.CalendarID
	if calID == 0 {
		if ev.AccountID == "" || ev.CalendarRemote == "" {
			return 0, fmt.Errorf("store: event needs calendar_id or account+calendar remote")
		}
		err := tx.q.QueryRowContext(ctx,
			`SELECT id FROM calendars WHERE account_id = ? AND remote_id = ?`,
			ev.AccountID, ev.CalendarRemote).Scan(&calID)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, notFound("calendar %s:%s", ev.AccountID, ev.CalendarRemote)
		}
		if err != nil {
			return 0, fmt.Errorf("store: resolve calendar: %w", err)
		}
	}

	organizer, err := marshalOne(ev.Organizer)
	if err != nil {
		return 0, err
	}
	var attendees any
	if len(ev.Attendees) > 0 {
		b, err := json.Marshal(ev.Attendees)
		if err != nil {
			return 0, fmt.Errorf("store: marshal attendees: %w", err)
		}
		attendees = string(b)
	}
	rawJSON := "{}"
	if len(ev.RawJSON) > 0 {
		rawJSON = string(ev.RawJSON)
	}

	var id int64
	err = tx.q.QueryRowContext(ctx, `
		INSERT INTO events (calendar_id, remote_id, uid, title, description, location,
			start_utc, end_utc, all_day, timezone, rrule, recurrence_id, status,
			organizer, attendees_json, my_response, raw_json, updated_utc, deleted_at)
		VALUES (?,?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?,?,?,?)
		ON CONFLICT(calendar_id, remote_id) DO UPDATE SET
		  uid = excluded.uid, title = excluded.title, description = excluded.description,
		  location = excluded.location, start_utc = excluded.start_utc, end_utc = excluded.end_utc,
		  all_day = excluded.all_day, timezone = excluded.timezone, rrule = excluded.rrule,
		  recurrence_id = excluded.recurrence_id, status = excluded.status,
		  organizer = excluded.organizer, attendees_json = excluded.attendees_json,
		  my_response = excluded.my_response, raw_json = excluded.raw_json,
		  updated_utc = excluded.updated_utc, deleted_at = excluded.deleted_at
		RETURNING id`,
		calID, ev.RemoteID, nullStr(ev.UID), nullStr(ev.Title), nullStr(ev.Description),
		nullStr(ev.Location), unixOf(ev.Start), unixOf(ev.End), boolInt(ev.AllDay),
		nullStr(ev.Timezone), nullStr(ev.RRule), nullStr(ev.RecurrenceID), nullStr(string(ev.Status)),
		organizer, attendees, nullStr(string(ev.MyResponse)), rawJSON,
		unixOf(ev.Updated), nullUnix(ev.DeletedAt)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: upsert event %s: %w", ev.RemoteID, err)
	}
	ev.ID = id
	ev.CalendarID = calID
	return id, nil
}

func marshalOne(a model.Address) (any, error) {
	if a.Email == "" && a.Name == "" {
		return nil, nil
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("store: marshal address: %w", err)
	}
	return string(b), nil
}

// MarkEventDeleted marks one event as removed on the server and drops its
// expanded occurrences so it disappears from the agenda.
func (s *Store) MarkEventDeleted(ctx context.Context, calendarID int64, remote string) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.MarkEventDeleted(ctx, calendarID, remote) })
}

func (tx *Tx) MarkEventDeleted(ctx context.Context, calendarID int64, remote string) error {
	var id int64
	err := tx.q.QueryRowContext(ctx,
		`SELECT id FROM events WHERE calendar_id = ? AND remote_id = ?`, calendarID, remote).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // already gone
	}
	if err != nil {
		return fmt.Errorf("store: mark event deleted: %w", err)
	}
	if _, err := tx.q.ExecContext(ctx,
		`UPDATE events SET deleted_at = ? WHERE id = ?`, time.Now().Unix(), id); err != nil {
		return fmt.Errorf("store: mark event deleted: %w", err)
	}
	if _, err := tx.q.ExecContext(ctx,
		`DELETE FROM event_occurrences WHERE event_id = ?`, id); err != nil {
		return fmt.Errorf("store: clear occurrences: %w", err)
	}
	return nil
}

// DeleteEventRow removes an event row outright, occurrences included (the FK
// cascades). This is not the "gone from the server" path — that is
// MarkEventDeleted, which keeps the row — it is for undoing a local write that
// never happened, e.g. the optimistically created event of a rejected
// `cal add`. A remote id that is not there is not an error.
func (s *Store) DeleteEventRow(ctx context.Context, calendarID int64, remote string) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.DeleteEventRow(ctx, calendarID, remote) })
}

func (tx *Tx) DeleteEventRow(ctx context.Context, calendarID int64, remote string) error {
	if _, err := tx.q.ExecContext(ctx,
		`DELETE FROM events WHERE calendar_id = ? AND remote_id = ?`, calendarID, remote); err != nil {
		return fmt.Errorf("store: delete event %s: %w", remote, err)
	}
	return nil
}

// GetEvent returns one event by public coordinates, or model.ErrNotFound.
func (s *Store) GetEvent(ctx context.Context, accountID, calendarRemote, remote string) (*model.Event, error) {
	return s.tx().GetEvent(ctx, accountID, calendarRemote, remote)
}

func (tx *Tx) GetEvent(ctx context.Context, accountID, calendarRemote, remote string) (*model.Event, error) {
	row := tx.q.QueryRowContext(ctx,
		`SELECT `+eventCols+eventFrom+
			` WHERE c.account_id = ? AND c.remote_id = ? AND e.remote_id = ?`,
		accountID, calendarRemote, remote)
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("event %s", model.EventPublicID(accountID, calendarRemote, remote))
	}
	if err != nil {
		return nil, fmt.Errorf("store: get event: %w", err)
	}
	return &e, nil
}

// GetEventByID returns one event by local row id.
func (s *Store) GetEventByID(ctx context.Context, id int64) (*model.Event, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+eventCols+eventFrom+` WHERE e.id = ?`, id)
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("event #%d", id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get event #%d: %w", id, err)
	}
	return &e, nil
}

// ListRecurringEvents returns every non-deleted event carrying an RRULE, for
// the calendar package to expand into event_occurrences.
func (s *Store) ListRecurringEvents(ctx context.Context, accountIDs []string) ([]model.Event, error) {
	q := `SELECT ` + eventCols + eventFrom +
		` WHERE e.deleted_at IS NULL AND e.rrule IS NOT NULL AND e.rrule <> ''`
	var args []any
	if len(accountIDs) > 0 {
		q += ` AND c.account_id IN (` + placeholders(len(accountIDs)) + `)`
		args = anySlice(accountIDs)
	}
	q += ` ORDER BY e.start_utc`
	return s.queryEvents(ctx, q, args...)
}

// ListEventExceptions returns the exception instances (events with a
// recurrence id) belonging to one calendar, so the expander can skip or
// override the generated occurrences.
func (s *Store) ListEventExceptions(ctx context.Context, calendarID int64) ([]model.Event, error) {
	return s.queryEvents(ctx,
		`SELECT `+eventCols+eventFrom+
			` WHERE e.calendar_id = ? AND e.recurrence_id IS NOT NULL AND e.recurrence_id <> ''
			  ORDER BY e.start_utc`, calendarID)
}

// ListEventsUpdatedSince returns events whose provider timestamp is newer than
// t (used to re-expand only what changed).
func (s *Store) ListEventsUpdatedSince(ctx context.Context, accountIDs []string, t time.Time) ([]model.Event, error) {
	q := `SELECT ` + eventCols + eventFrom + ` WHERE e.updated_utc >= ?`
	args := []any{unixOf(t)}
	if len(accountIDs) > 0 {
		q += ` AND c.account_id IN (` + placeholders(len(accountIDs)) + `)`
		args = append(args, anySlice(accountIDs)...)
	}
	q += ` ORDER BY e.updated_utc`
	return s.queryEvents(ctx, q, args...)
}

func (s *Store) queryEvents(ctx context.Context, q string, args ...any) ([]model.Event, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query events: %w", err)
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Occurrences

// ReplaceOccurrences rewrites the expanded instances of one event.
func (s *Store) ReplaceOccurrences(ctx context.Context, eventID int64, occs []model.Occurrence) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.ReplaceOccurrences(ctx, eventID, occs) })
}

func (tx *Tx) ReplaceOccurrences(ctx context.Context, eventID int64, occs []model.Occurrence) error {
	if _, err := tx.q.ExecContext(ctx,
		`DELETE FROM event_occurrences WHERE event_id = ?`, eventID); err != nil {
		return fmt.Errorf("store: clear occurrences: %w", err)
	}
	for _, o := range occs {
		if _, err := tx.q.ExecContext(ctx,
			`INSERT OR REPLACE INTO event_occurrences (event_id, start_utc, end_utc) VALUES (?,?,?)`,
			eventID, unixOf(o.Start), unixOf(o.End)); err != nil {
			return fmt.Errorf("store: insert occurrence: %w", err)
		}
	}
	return nil
}

// OccurrenceRow is one expanded instance joined with the summary fields the
// agenda needs, so `cal agenda` is a single query.
type OccurrenceRow struct {
	model.Occurrence

	AccountID      string              `json:"account"`
	CalendarID     int64               `json:"calendar_id"`
	CalendarRemote string              `json:"calendar_remote"`
	CalendarName   string              `json:"calendar_name"`
	EventRemoteID  string              `json:"event_remote"`
	Title          string              `json:"title"`
	Location       string              `json:"location,omitempty"`
	AllDay         bool                `json:"all_day"`
	Timezone       string              `json:"timezone,omitempty"`
	Recurring      bool                `json:"recurring"`
	Status         model.EventStatus   `json:"status,omitempty"`
	MyResponse     model.Participation `json:"my_response,omitempty"`
	Organizer      model.Address       `json:"organizer,omitempty"`
}

// PublicID returns the id of the underlying event.
func (o *OccurrenceRow) PublicID() string {
	return model.EventPublicID(o.AccountID, o.CalendarRemote, o.EventRemoteID)
}

// ListOccurrences returns every expanded instance overlapping [from, to),
// ordered by start. calendarIDs empty means all calendars. Deleted events are
// excluded; cancelled ones are returned so callers can show them struck out.
func (s *Store) ListOccurrences(ctx context.Context, from, to time.Time, calendarIDs []int64) ([]OccurrenceRow, error) {
	return s.tx().ListOccurrences(ctx, from, to, calendarIDs)
}

func (tx *Tx) ListOccurrences(ctx context.Context, from, to time.Time, calendarIDs []int64) ([]OccurrenceRow, error) {
	q := `SELECT o.event_id, o.start_utc, o.end_utc,
	             c.account_id, c.id, c.remote_id, c.name,
	             e.remote_id, e.title, e.location, e.all_day, e.timezone,
	             e.rrule, e.status, e.my_response, e.organizer
	        FROM event_occurrences o
	        JOIN events e ON e.id = o.event_id
	        JOIN calendars c ON c.id = e.calendar_id
	       WHERE e.deleted_at IS NULL AND o.start_utc < ? AND o.end_utc > ?`
	args := []any{unixOf(to), unixOf(from)}
	if len(calendarIDs) > 0 {
		q += ` AND c.id IN (` + placeholders(len(calendarIDs)) + `)`
		args = append(args, anySlice(calendarIDs)...)
	}
	q += ` ORDER BY o.start_utc, e.title`

	rows, err := tx.q.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list occurrences: %w", err)
	}
	defer rows.Close()
	var out []OccurrenceRow
	for rows.Next() {
		var r OccurrenceRow
		var start, end int64
		var title, loc, tz, rrule, status, myResp, organizer sql.NullString
		var allDay int64
		if err := rows.Scan(&r.EventID, &start, &end,
			&r.AccountID, &r.CalendarID, &r.CalendarRemote, &r.CalendarName,
			&r.EventRemoteID, &title, &loc, &allDay, &tz,
			&rrule, &status, &myResp, &organizer); err != nil {
			return nil, err
		}
		r.Start = timeOf(start)
		r.End = timeOf(end)
		r.Title = title.String
		r.Location = loc.String
		r.AllDay = allDay != 0
		r.Timezone = tz.String
		r.Recurring = rrule.Valid && rrule.String != ""
		r.Status = model.EventStatus(status.String)
		r.MyResponse = model.Participation(myResp.String)
		if organizer.Valid && organizer.String != "" {
			_ = json.Unmarshal([]byte(organizer.String), &r.Organizer)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountOccurrences reports how many expanded instances one event has (a cheap
// way for the expander to see whether work is needed).
func (s *Store) CountOccurrences(ctx context.Context, eventID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM event_occurrences WHERE event_id = ?`, eventID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count occurrences: %w", err)
	}
	return n, nil
}

// OccurrenceWindow returns the earliest and latest expanded instance across
// the given calendars, so the nightly job knows how far the window reaches.
func (s *Store) OccurrenceWindow(ctx context.Context, calendarIDs []int64) (first, last time.Time, err error) {
	q := `SELECT min(o.start_utc), max(o.start_utc)
	        FROM event_occurrences o JOIN events e ON e.id = o.event_id`
	var args []any
	if len(calendarIDs) > 0 {
		q += ` WHERE e.calendar_id IN (` + placeholders(len(calendarIDs)) + `)`
		args = anySlice(calendarIDs)
	}
	var lo, hi sql.NullInt64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&lo, &hi); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("store: occurrence window: %w", err)
	}
	return nullTime(lo), nullTime(hi), nil
}
