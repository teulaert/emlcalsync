package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
)

// staleCheck is a check well in the past, planted so a test can see a pass
// move it: the engine stamps time.Now(), and two syncs in one test land in
// the same second.
var staleCheck = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func (e *testEnv) plantCheck(account, resource string) {
	e.T.Helper()
	app, _, _ := e.App()
	defer app.Close()
	st, err := app.Store()
	if err != nil {
		e.T.Fatal(err)
	}
	if err := st.RecordSyncCheck(context.Background(), account, resource, staleCheck, nil); err != nil {
		e.T.Fatal(err)
	}
}

func (e *testEnv) statusRow(account string) coreStatusRow {
	e.T.Helper()
	out := coreDecodeOne[coreStatusOut](e.T, e.MustRun("status"))
	for _, r := range out.Accounts {
		if r.Name == account {
			return r
		}
	}
	e.T.Fatalf("status has no row for %s: %+v", account, out.Accounts)
	return coreStatusRow{}
}

// The ticket (#3): a watch service polling a quiet mailbox showed a
// last_sync hours old, and a monitor reading it concluded mail was not being
// checked. Status now says when each resource was last checked, and a pass
// that found nothing counts.
func TestStatusNoOpSyncAdvancesCheckedNotChanged(t *testing.T) {
	env := newTestEnv(t)
	env.Seed("work", fake.NewMsg("m1", RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now)))
	before := env.statusRow("work")
	if before.Mail.ChangedAt.IsZero() {
		t.Fatalf("the seeding sync was not a change: %+v", before.Mail)
	}
	env.plantCheck("work", "mail")

	env.MustRun("sync", "--account", "work")
	after := env.statusRow("work")

	if !after.Mail.CheckedAt.After(staleCheck) {
		t.Errorf("a no-op sync did not move mail checked_at: %+v", after.Mail)
	}
	if !after.Mail.ChangedAt.Equal(before.Mail.ChangedAt.Time) || after.Mail.ChangeKind != before.Mail.ChangeKind {
		t.Errorf("a no-op sync moved mail changed_at from %+v to %+v", before.Mail, after.Mail)
	}
	if after.Mail.Error != "" || !after.Mail.FailedAt.IsZero() {
		t.Errorf("a no-op sync was reported as a failure: %+v", after.Mail)
	}
	if after.Mail.CheckedAtUTC != after.Mail.CheckedAt.Unix() || after.Mail.ChangedAtUTC != after.Mail.ChangedAt.Unix() {
		t.Errorf("_utc companions disagree with their times: %+v", after.Mail)
	}
}

// A calendar that keeps changing must not make a mail check look fresh.
func TestStatusCalendarChangeDoesNotMaskAStaleMailCheck(t *testing.T) {
	env := newTestEnv(t)
	env.Seed("work", fake.NewMsg("m1", RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now)))
	env.plantCheck("work", "mail")
	start := env.Now.Add(24 * time.Hour)
	env.Cal["work"].Put("primary", model.Event{
		RemoteID: "e1", UID: "u1", Title: "Standup", Timezone: "UTC",
		Start: start, End: start.Add(time.Hour), Status: model.StatusConfirmed,
	})

	env.MustRun("sync", "--account", "work", "--calendar-only")
	row := env.statusRow("work")

	if !row.Mail.CheckedAt.Equal(staleCheck) {
		t.Errorf("a calendar-only sync moved the mail check: %+v", row.Mail)
	}
	if row.Calendar.CheckedAt.IsZero() || row.Calendar.ChangedAt.IsZero() || row.Calendar.ChangeKind != "calendar" {
		t.Errorf("the calendar pass left no check or change: %+v", row.Calendar)
	}
	if row.Calendar.CheckedAt.Equal(row.Mail.CheckedAt.Time) {
		t.Errorf("the two checks are the same time: mail %s calendar %s", row.Mail.CheckedAt, row.Calendar.CheckedAt)
	}
	// The mail backfill and the calendar pass may land in the same second, so
	// the later of the two is asserted by time, not by kind.
	if !row.LastChange.Equal(row.Calendar.ChangedAt.Time) {
		t.Errorf("last_change = %s, want the calendar's %s", row.LastChange, row.Calendar.ChangedAt)
	}
}

// A failed pass stays a failure in the output: the last good check is kept,
// and the failure is named beside it, in JSON and in the table.
func TestStatusFailedPassIsToldApartFromACheck(t *testing.T) {
	env := newTestEnv(t)
	env.Seed("work", fake.NewMsg("m1", RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now)))
	env.plantCheck("work", "mail")
	env.Mail["work"].FailNext(1)

	if _, _, code := env.Run("sync", "--account", "work", "--mail-only"); code == 0 {
		t.Fatal("sync against a failing provider exited 0")
	}
	row := env.statusRow("work")

	if !row.Mail.CheckedAt.Equal(staleCheck) {
		t.Errorf("a failed pass moved checked_at: %+v", row.Mail)
	}
	if row.Mail.Error == "" || !row.Mail.FailedAt.After(staleCheck) {
		t.Errorf("the failure is not on the row: %+v", row.Mail)
	}
	table := env.MustRun("status", "-o", "table")
	for _, want := range []string{"MAIL CHECKED", "CAL CHECKED", "LAST CHANGE", "ERROR", "mail: "} {
		// The test terminal is narrow and squeezes the headers; the first
		// letters are what reliably fit.
		want = want[:min(len(want), 4)]
		if !strings.Contains(table, want) {
			t.Errorf("table status does not contain %q:\n%s", want, table)
		}
	}
	if strings.Contains(table, "LAST SYNC") {
		t.Errorf("the table still has a LAST SYNC column:\n%s", table)
	}
}
