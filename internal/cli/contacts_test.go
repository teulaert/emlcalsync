package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
)

// namedMail is RawMail with display names on both ends.
func namedMail(t *testing.T, from, to model.Address, subject string, date time.Time) []byte {
	t.Helper()
	raw, err := mime.Build(&mime.Draft{
		From: from, To: []model.Address{to}, Subject: subject, TextBody: subject,
		Date: date, MessageID: strings.NewReplacer(" ", "-").Replace(subject) + "@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// contactsSeed is an archive with one person written to and two who wrote in.
// The fake provider has no sent mailbox, so what marks a message as the
// account's own is its From.
func contactsSeed(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	me := model.Address{Email: "me@example.com"}
	anna := model.Address{Name: "Anna de Vries", Email: "anna@example.com"}
	env.Seed("work",
		fake.NewMsg("s-1", namedMail(t, me, anna, "offerte", env.Now.Add(-3*24*time.Hour))),
		fake.NewMsg("in-1", namedMail(t, anna, me, "Re: offerte", env.Now.Add(-2*24*time.Hour))),
		fake.NewMsg("in-2", RawMail(t, "bob@example.com", "me@example.com", "hello", "hi", env.Now.Add(-time.Hour))),
		fake.NewMsg("in-3", RawMail(t, "bob@example.com", "me@example.com", "hello again", "hi", env.Now.Add(-30*time.Minute))),
		fake.NewMsg("in-4", RawMail(t, "noreply@shop.example", "me@example.com", "your order", "…", env.Now)),
	)
	env.Seed("home",
		fake.NewMsg("h-1", RawMail(t, "bob@example.com", "me@gmail.example", "weekend", "?", env.Now.Add(-time.Hour))),
	)
	return env
}

type contactJSON struct {
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	Address  string   `json:"address"`
	Sent     int      `json:"sent"`
	Messages int      `json:"messages"`
	Last     string   `json:"last"`
	Accounts []string `json:"accounts"`
}

func contactsList(t *testing.T, env *testEnv, args ...string) []contactJSON {
	t.Helper()
	out := env.MustRun(append([]string{"contacts"}, args...)...)
	var rows []contactJSON
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("contacts %s: bad JSON %v:\n%s", strings.Join(args, " "), err, out)
	}
	return rows
}

func TestContactsListRanksThePeopleYouWriteTo(t *testing.T) {
	env := contactsSeed(t)
	rows := contactsList(t, env, "list")
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want anna then bob", rows)
	}
	anna, bob := rows[0], rows[1]
	if anna.Email != "anna@example.com" || anna.Sent != 1 || anna.Messages != 2 || anna.Name != "Anna de Vries" {
		t.Errorf("anna = %+v", anna)
	}
	if anna.Address != "Anna de Vries <anna@example.com>" {
		t.Errorf("address = %q, want something --to takes", anna.Address)
	}
	if bob.Email != "bob@example.com" || bob.Sent != 0 || bob.Messages != 3 || strings.Join(bob.Accounts, ",") != "home,work" {
		t.Errorf("bob = %+v", bob)
	}
	if bob.Address != "bob@example.com" {
		t.Errorf("a nameless address stays bare: %q", bob.Address)
	}
	if !strings.HasPrefix(anna.Last, "2026-08-23") {
		t.Errorf("last = %q, want the day of her reply", anna.Last)
	}

	rows = contactsList(t, env, "list", "--account", "home")
	if len(rows) != 1 || rows[0].Email != "bob@example.com" || rows[0].Messages != 1 {
		t.Errorf("--account home = %+v", rows)
	}
	if rows := contactsList(t, env, "list", "--limit", "1"); len(rows) != 1 || rows[0].Email != "anna@example.com" {
		t.Errorf("--limit 1 = %+v", rows)
	}
	if _, _, code := env.Run("contacts", "list", "--account", "nope"); code != 3 {
		t.Errorf("unknown account exit = %d, want 3", code)
	}
}

func TestContactsSearchMatchesNameOrAddress(t *testing.T) {
	env := contactsSeed(t)
	if rows := contactsList(t, env, "search", "VRIES"); len(rows) != 1 || rows[0].Email != "anna@example.com" {
		t.Errorf("search vries = %+v", rows)
	}
	if rows := contactsList(t, env, "search", "bob@"); len(rows) != 1 || rows[0].Email != "bob@example.com" {
		t.Errorf("search bob@ = %+v", rows)
	}
	out := env.MustRun("contacts", "search", "nobody")
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("nobody found is an empty list, got %q", out)
	}
}

func TestContactsSearchNeedsAQuery(t *testing.T) {
	env := contactsSeed(t)
	if _, _, code := env.Run("contacts", "search"); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestContactsListAsATable(t *testing.T) {
	env := contactsSeed(t)
	out := env.MustRun("contacts", "list", "-o", "table")
	head := strings.SplitN(out, "\n", 2)[0]
	for _, col := range []string{"NAME", "EMAIL", "SENT", "MSGS", "LAST", "ACCOUNTS"} {
		if !strings.Contains(head, col) {
			t.Errorf("header %q lacks %s", head, col)
		}
	}
	if !strings.Contains(out, "Anna de Vries") || !strings.Contains(out, "home, work") {
		t.Errorf("table:\n%s", out)
	}
}
