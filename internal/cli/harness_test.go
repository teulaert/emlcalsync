package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// fakeFactory hands out the in-memory providers per account name.
type fakeFactory struct {
	Mails map[string]*fake.Mail
	Cals  map[string]*fake.Calendar
}

func (f *fakeFactory) Mail(ctx context.Context, acct config.Account) (provider.MailProvider, error) {
	m, ok := f.Mails[acct.Name]
	if !ok {
		return nil, fmt.Errorf("no fake mail for %s", acct.Name)
	}
	return m, nil
}
func (f *fakeFactory) Calendar(ctx context.Context, acct config.Account) (provider.CalendarProvider, error) {
	c, ok := f.Cals[acct.Name]
	if !ok {
		return nil, fmt.Errorf("no fake calendar for %s", acct.Name)
	}
	return c, nil
}
func (f *fakeFactory) Pusher(ctx context.Context, acct config.Account) (provider.Pusher, bool, error) {
	m, ok := f.Mails[acct.Name]
	return m, ok, nil
}

// testEnv is an App wired to temp XDG dirs and fake providers.
type testEnv struct {
	T      *testing.T
	Dir    string
	Config string
	Mail   map[string]*fake.Mail
	Cal    map[string]*fake.Calendar
	Stdin  string
	Now    time.Time
	// Opened collects the URLs a command would have handed the desktop, so a
	// test can press the escape hatch without a browser appearing.
	Opened  []string
	factory *fakeFactory
}

// newTestEnv writes a config.toml with the given accounts into a temp dir and
// returns the environment. Accounts default to one fastmail account "work"
// <me@example.com> and one gmail "home".
//
// Accounts are built with config.NewAccount because the zero Account has no
// resource blocks, and so would sync nothing.
func newTestEnv(t *testing.T, accounts ...config.Account) *testEnv {
	t.Helper()
	if len(accounts) == 0 {
		accounts = []config.Account{
			config.NewAccount("work", "me@example.com", model.VendorFastmail),
			config.NewAccount("home", "me@gmail.example", model.VendorGoogle),
		}
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("HOME", dir)
	t.Setenv(config.EnvFormat, "")
	t.Setenv(config.EnvConfig, "")
	t.Setenv(config.EnvDataDir, "")
	cfg := config.Default()
	cfg.General.Timezone = "UTC"
	cfg.Accounts = accounts
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Poll == 0 {
			cfg.Accounts[i].Poll = config.Duration(time.Hour)
		}
	}
	path := filepath.Join(dir, "config", "emlcal", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	env := &testEnv{T: t, Dir: dir, Config: path, Mail: map[string]*fake.Mail{}, Cal: map[string]*fake.Calendar{},
		Now: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)}
	for _, a := range accounts {
		env.Mail[a.Name] = fake.NewMail()
		env.Cal[a.Name] = fake.NewCalendar()
	}
	env.factory = &fakeFactory{Mails: env.Mail, Cals: env.Cal}
	return env
}

// App builds a fresh App (one per command invocation, like a real process).
func (e *testEnv) App() (*App, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	app := &App{
		Stdout:     &out,
		Stderr:     &errb,
		Stdin:      strings.NewReader(e.Stdin),
		IsTTY:      false,
		Now:        func() time.Time { return e.Now },
		ConfigPath: e.Config,
		Factory:    e.factory,
		OpenBrowser: func(url string) error {
			e.Opened = append(e.Opened, url)
			return nil
		},
	}
	return app, &out, &errb
}

// Run executes one command line and returns stdout, stderr and the exit code.
func (e *testEnv) Run(args ...string) (stdout, stderr string, code int) {
	e.T.Helper()
	app, out, errb := e.App()
	code = Execute(args, app)
	return out.String(), errb.String(), code
}

// MustRun fails the test unless the command exits 0; returns stdout.
func (e *testEnv) MustRun(args ...string) string {
	e.T.Helper()
	out, errs, code := e.Run(args...)
	if code != 0 {
		e.T.Fatalf("emlcal %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, out, errs)
	}
	return out
}

// Sync runs the engine directly (faster than the command, no output).
func (e *testEnv) Sync(account string) {
	e.T.Helper()
	app, _, _ := e.App()
	defer app.Close()
	eng, err := app.Engine()
	if err != nil {
		e.T.Fatal(err)
	}
	if _, err := eng.SyncAccount(context.Background(), account, sync.SyncOptions{}); err != nil {
		e.T.Fatalf("sync %s: %v", account, err)
	}
}

// Seed adds messages to an account's fake provider and syncs.
func (e *testEnv) Seed(account string, msgs ...*fake.Msg) {
	e.T.Helper()
	for _, m := range msgs {
		e.Mail[account].Add(m)
	}
	e.Sync(account)
}

// RawMail builds an RFC 822 message for seeding.
func RawMail(t *testing.T, from, to, subject, body string, date time.Time) []byte {
	t.Helper()
	raw, err := mime.Build(&mime.Draft{
		From:      model.Address{Email: from},
		To:        []model.Address{{Email: to}},
		Subject:   subject,
		TextBody:  body,
		Date:      date,
		MessageID: fmt.Sprintf("%d.%s@example.test", date.Unix(), strings.NewReplacer(" ", "-", "@", "-").Replace(subject)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestHarnessSmoke(t *testing.T) {
	env := newTestEnv(t)
	env.Seed("work", fake.NewMsg("m1", RawMail(t, "alice@example.com", "me@example.com", "Hello", "hi there", env.Now.Add(-time.Hour))))
	out := env.MustRun("--help")
	if !strings.Contains(out, "emlcal") {
		t.Fatalf("help output: %s", out)
	}
	_, _, code := env.Run("definitely-not-a-command")
	if code != 2 {
		t.Fatalf("unknown command exit = %d", code)
	}
}
