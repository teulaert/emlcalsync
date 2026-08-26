package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/provider/caldav"
	"github.com/teulaert/emlcalsync/internal/provider/caldav/caldavfake"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
)

// coreSecretValue reads one secrets file from the environment's config dir.
func coreSecretValue(t *testing.T, env *testEnv, key string) (string, bool) {
	t.Helper()
	cfg, err := config.Load(env.Config)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfg.SecretsDir(), key))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func TestAccountAddFastmailWithAppPassword(t *testing.T) {
	env := newTestEnv(t)
	env.Mail["extra"] = fake.NewMail()
	env.Cal["extra"] = fake.NewCalendar()
	// The token comes first, then the app password, one line each.
	env.Stdin = "secret-token\napp-pass-1234\n"

	out := env.MustRun("account", "add", "fastmail", "--name", "extra",
		"--email", "x@y.example", "--token-stdin", "--app-password-stdin")

	row := coreDecodeOne[map[string]any](t, out)
	if row["calendar_api"] != "caldav" {
		t.Errorf("calendar_api = %v, want caldav: %v", row["calendar_api"], row)
	}
	if row["calendars"] != float64(1) {
		t.Errorf("calendars = %v, want the one calendar the provider reported", row["calendars"])
	}

	if got, ok := coreSecretValue(t, env, "extra.caldav.password"); !ok || got != "app-pass-1234" {
		t.Errorf("app password secret = %q (present=%v)", got, ok)
	}
	if got, ok := coreSecretValue(t, env, "extra.jmap.token"); !ok || got != "secret-token" {
		t.Errorf("token secret = %q (present=%v)", got, ok)
	}
}

func TestAccountAddFastmailWithAppPasswordFlag(t *testing.T) {
	env := newTestEnv(t)
	env.Mail["extra"] = fake.NewMail()
	env.Cal["extra"] = fake.NewCalendar()
	env.Stdin = "secret-token\n"

	env.MustRun("account", "add", "fastmail", "--name", "extra",
		"--email", "x@y.example", "--token-stdin", "--app-password", "from-flag")

	if got, _ := coreSecretValue(t, env, "extra.caldav.password"); got != "from-flag" {
		t.Errorf("app password secret = %q", got)
	}
}

func TestAccountAddFastmailWithoutAppPasswordHasNoCalendars(t *testing.T) {
	env := newTestEnv(t)
	env.Mail["extra"] = fake.NewMail()
	env.Cal["extra"] = fake.NewCalendar()
	env.Stdin = "secret-token\n"

	out := env.MustRun("account", "add", "fastmail", "--name", "extra",
		"--email", "x@y.example", "--token-stdin")

	row := coreDecodeOne[map[string]any](t, out)
	if row["calendar_api"] != "none" {
		t.Errorf("calendar_api = %v, want none", row["calendar_api"])
	}
	if _, ok := coreSecretValue(t, env, "extra.caldav.password"); ok {
		t.Error("an app password was stored even though none was given")
	}
}

func TestAccountAddFastmailRejectsBothPasswordFlags(t *testing.T) {
	env := newTestEnv(t)
	env.Stdin = "secret-token\n"
	_, errs, code := env.Run("account", "add", "fastmail", "--name", "extra",
		"--email", "x@y.example", "--token-stdin", "--app-password", "x", "--app-password-stdin")
	if code == 0 || !strings.Contains(errs, "mutually exclusive") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

func TestAccountCalDAVPasswordCommand(t *testing.T) {
	env := newTestEnv(t)
	env.Stdin = "later-app-pass\n"

	out := env.MustRun("account", "caldav-password", "--name", "work", "--stdin")
	row := coreDecodeOne[map[string]any](t, out)
	if row["stored"] != true || row["calendar_api"] != "caldav" {
		t.Errorf("row = %v", row)
	}
	if row["calendars"] != float64(1) {
		t.Errorf("calendars = %v, want the verification count", row["calendars"])
	}
	if got, _ := coreSecretValue(t, env, "work.caldav.password"); got != "later-app-pass" {
		t.Errorf("app password secret = %q", got)
	}

	// It now shows up in `account list`.
	rows := coreDecodeRows[map[string]any](t, env.MustRun("account", "list"))
	for _, r := range rows {
		if r["name"] == "work" && r["calendar_api"] != "caldav" {
			t.Errorf("account list = %v", r)
		}
	}
}

// A Google account's calendars come over the Calendar API, so a CalDAV password
// would be written where nothing reads it. The old `fastmail-password` spelling
// still works as an alias.
func TestAccountCalDAVPasswordRejectsANonCalDAVAccount(t *testing.T) {
	env := newTestEnv(t)
	for _, cmd := range []string{"caldav-password", "fastmail-password"} {
		_, errs, code := env.Run("account", cmd, "--name", "home", "--app-password", "x")
		if code == 0 || !strings.Contains(errs, "CalDAV calendar backend") {
			t.Fatalf("%s: exit %d, stderr %q", cmd, code, errs)
		}
	}
}

func TestAccountListShowsCalendarBackend(t *testing.T) {
	env := newTestEnv(t)
	rows := coreDecodeRows[map[string]any](t, env.MustRun("account", "list"))
	got := map[string]any{}
	for _, r := range rows {
		got[r["name"].(string)] = r["calendar_api"]
	}
	if got["work"] != "none" {
		t.Errorf("fastmail without an app password = %v, want none", got["work"])
	}
	if got["home"] != "gcal" {
		t.Errorf("gmail = %v, want gcal", got["home"])
	}
}

// ---------------------------------------------------------------------------
// the real Factory

// realFactoryApp builds an App wired to the production Factory (not the fake
// providers the rest of the CLI tests use).
func realFactoryApp(t *testing.T, env *testEnv) *App {
	t.Helper()
	app, _, _ := env.App()
	app.Factory = &Factory{app: app}
	t.Cleanup(func() { app.Close() })
	return app
}

func TestFactoryUsesCalDAVWhenAnAppPasswordIsStored(t *testing.T) {
	env := newTestEnv(t, config.NewAccount("work", "me@example.com", model.VendorFastmail))
	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.User, srv.Password = "me@example.com", "app-pass"
	srv.AddCalendar(caldavfake.Calendar{
		Path: srv.HomePath("me@example.com") + "Default/", Name: "Calendar", Color: "#112233",
	})
	srv.AddCalendar(caldavfake.Calendar{
		Path: srv.HomePath("me@example.com") + "other/", Name: "Side projects",
	})
	t.Setenv(EnvCalDAVBaseURL, srv.BaseURL())

	app := realFactoryApp(t, env)
	sec, err := app.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	acct, err := app.ResolveAccount("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := sec.Set(CalDAVPasswordKey(*acct), []byte("app-pass")); err != nil {
		t.Fatal(err)
	}

	cp, err := app.Factory.Calendar(context.Background(), *acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cp.(*caldav.Calendar); !ok {
		t.Fatalf("calendar provider = %T, want *caldav.Calendar", cp)
	}
	cals, err := cp.Calendars(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 2 || cals[0].Name != "Calendar" || cals[0].Color != "#112233" {
		t.Fatalf("calendars = %+v", cals)
	}
}

// A CalDAV calendar with no stored password is reported as unsupported, not as
// an error: the sync engine skips the resource and the rest of the account
// keeps working, which is what a half-configured account should do.
func TestFactoryReportsCalDAVWithoutAPasswordAsUnsupported(t *testing.T) {
	env := newTestEnv(t, config.NewAccount("work", "me@example.com", model.VendorFastmail))
	app := realFactoryApp(t, env)
	sec, err := app.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	acct, err := app.ResolveAccount("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := sec.Set(JMAPTokenKey(*acct), []byte("token")); err != nil {
		t.Fatal(err)
	}

	_, err = app.Factory.Calendar(context.Background(), *acct)
	if !errors.Is(err, provider.ErrNotSupported) {
		t.Fatalf("Calendar error = %v, want provider.ErrNotSupported", err)
	}
	if !strings.Contains(err.Error(), "caldav-password") {
		t.Errorf("error %q should name the command that fixes it", err)
	}
}

func TestPerAccountGoogleClientWins(t *testing.T) {
	env := newTestEnv(t)
	app := realFactoryApp(t, env)
	sec, err := app.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	write := func(key, id, secret string) {
		b, _ := json.Marshal(map[string]string{"client_id": id, "client_secret": secret})
		if err := sec.Set(key, b); err != nil {
			t.Fatal(err)
		}
	}
	const globalID = "1-global.apps.googleusercontent.com"
	const perAcctID = "2-peracct.apps.googleusercontent.com"
	write(GoogleClientKey, globalID, "GOCSPX-global")

	acct, err := app.ResolveAccount("home")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := app.GoogleOAuthConfig(*acct)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientID != globalID {
		t.Fatalf("without a per-account client the shared one must be used, got %q", cfg.ClientID)
	}

	write(GoogleClientKeyFor(*acct), perAcctID, "GOCSPX-peracct")
	cfg, err = app.GoogleOAuthConfig(*acct)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientID != perAcctID || cfg.ClientSecret != "GOCSPX-peracct" {
		t.Fatalf("per-account client did not win: %+v", cfg)
	}

	// Another account keeps the shared client.
	other, err := app.ResolveAccount("work")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = app.GoogleOAuthConfig(*other)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientID != globalID {
		t.Fatalf("the per-account client leaked to %q: %+v", other.Name, cfg)
	}
}

func TestGoogleClientFlagsAreValidated(t *testing.T) {
	env := newTestEnv(t)
	_, errs, code := env.Run("account", "add", "gmail", "--name", "extra",
		"--email", "x@gmail.example", "--client-id", "not-a-client-id", "--client-secret", "GOCSPX-x")
	if code == 0 || !strings.Contains(errs, "--client-id") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	_, errs, code = env.Run("account", "add", "gmail", "--name", "extra",
		"--email", "x@gmail.example", "--client-id", "1-a.apps.googleusercontent.com")
	if code == 0 || !strings.Contains(errs, "together") {
		t.Fatalf("half a client pair must be refused: exit %d, stderr %q", code, errs)
	}
}

func TestAccountRemoveDeletesTheAppPassword(t *testing.T) {
	env := newTestEnv(t)
	env.Stdin = "app-pass\n"
	env.MustRun("account", "fastmail-password", "--name", "work", "--stdin")
	if _, ok := coreSecretValue(t, env, "work.caldav.password"); !ok {
		t.Fatal("setup: the app password was not stored")
	}
	env.MustRun("account", "remove", "work", "--yes")
	if _, ok := coreSecretValue(t, env, "work.caldav.password"); ok {
		t.Error("the app password survived `account remove`")
	}
}

// ---------------------------------------------------------------------------
// iCloud

// An iCloud account syncs calendars only: iCloud offers mail over IMAP, which
// emlcal does not speak, so the account is written with no [accounts.mail]
// block at all rather than a mail half that is switched off.
func TestAccountAddICloudIsCalendarOnly(t *testing.T) {
	env := newTestEnv(t)
	env.Cal["apple"] = fake.NewCalendar()

	env.Stdin = "app-specific\n"
	out := env.MustRun("account", "add", "icloud",
		"--name", "apple", "--email", "me@icloud.example", "--app-password-stdin")

	row := coreDecodeOne[map[string]any](t, out)
	if row["mail_api"] != "-" {
		t.Errorf("mail_api = %v, want %q: iCloud has no mail backend", row["mail_api"], "-")
	}
	if row["calendar_api"] != "caldav" {
		t.Errorf("calendar_api = %v, want caldav", row["calendar_api"])
	}
	if got, _ := coreSecretValue(t, env, "apple.caldav.password"); got != "app-specific" {
		t.Errorf("app password secret = %q", got)
	}

	cfg, err := config.Load(env.Config)
	if err != nil {
		t.Fatal(err)
	}
	acct, ok := cfg.Account("apple")
	if !ok {
		t.Fatal("the account was not written")
	}
	if acct.SyncsMail() {
		t.Error("an iCloud account must have no mail block")
	}
	if acct.Calendar == nil || acct.Calendar.Vendor != model.VendorICloud {
		t.Errorf("calendar block = %+v, want the icloud vendor", acct.Calendar)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the config it wrote does not validate: %v", err)
	}
}

// Without the password the account would sync nothing at all, so `add` refuses
// rather than leaving a dead entry behind.
func TestAccountAddICloudRequiresAnAppPassword(t *testing.T) {
	env := newTestEnv(t)
	env.Stdin = "\n"
	_, errs, code := env.Run("account", "add", "icloud",
		"--name", "apple", "--email", "me@icloud.example")
	if code == 0 || !strings.Contains(errs, "calendars only") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

// The Apple ID that authenticates is often not the iCloud address, so --username
// is what the CalDAV client presents while --email stays the account's identity.
func TestAccountAddICloudUsername(t *testing.T) {
	env := newTestEnv(t)
	env.Cal["apple"] = fake.NewCalendar()

	env.Stdin = "app-specific\n"
	env.MustRun("account", "add", "icloud", "--name", "apple",
		"--email", "me@icloud.example", "--username", "apple-id@example.com",
		"--app-password-stdin")

	cfg, err := config.Load(env.Config)
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := cfg.Account("apple")
	if acct.Calendar.Username != "apple-id@example.com" {
		t.Errorf("username = %q", acct.Calendar.Username)
	}
	if acct.Email != "me@icloud.example" {
		t.Errorf("email = %q; the address must not be overwritten by the Apple ID", acct.Email)
	}
}

// End to end through the production Factory: an iCloud account reaches a real
// CalDAV client, against an iCloud-shaped server (numeric account id, no
// guessable home path) that authenticates the Apple ID rather than the address.
func TestFactoryBuildsAnICloudCalDAVClient(t *testing.T) {
	acct := config.NewAccount("apple", "me@icloud.example", model.VendorICloud)
	acct.Calendar.Username = "apple-id@example.com"
	env := newTestEnv(t, acct)

	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.Root = "/"
	srv.User, srv.Password = "apple-id@example.com", "app-specific"
	srv.Principal, srv.Home = "/1234567890/principal/", "/1234567890/calendars/"
	srv.AddCalendar(caldavfake.Calendar{Path: srv.Home + "home/", Name: "Home"})
	srv.AddCalendar(caldavfake.Calendar{Path: srv.Home + "work/", Name: "Work"})
	t.Setenv(EnvCalDAVBaseURL, srv.BaseURL())

	app := realFactoryApp(t, env)
	sec, err := app.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := app.ResolveAccount("apple")
	if err != nil {
		t.Fatal(err)
	}
	if err := sec.Set(CalDAVPasswordKey(*resolved), []byte("app-specific")); err != nil {
		t.Fatal(err)
	}

	cp, err := app.Factory.Calendar(context.Background(), *resolved)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cp.(*caldav.Calendar); !ok {
		t.Fatalf("calendar provider = %T, want *caldav.Calendar", cp)
	}
	cals, err := cp.Calendars(context.Background())
	if err != nil {
		t.Fatalf("Calendars: %v", err)
	}
	if len(cals) != 2 {
		t.Fatalf("calendars = %+v, want both", cals)
	}
	if !cals[0].Primary || cals[0].Name != "Home" {
		t.Errorf(`want "Home" flagged primary, got %+v`, cals)
	}

	// An iCloud account has no mail half, and the engine must be told that in
	// the way it already knows how to skip.
	if _, err := app.Factory.Mail(context.Background(), *resolved); !errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("Mail error = %v, want provider.ErrNotSupported", err)
	}
}

// The online probe reaches a calendar-only account through its calendars.
// Asking such an account for mailboxes reported the absent mail block as a
// failure, which is the configuration working as intended.
func TestDoctorProbesCalendarsOnAMailLessAccount(t *testing.T) {
	acct := config.NewAccount("apple", "me@icloud.example", model.VendorICloud)
	env := newTestEnv(t, acct)
	env.Cal["apple"] = fake.NewCalendar()

	out := env.MustRun("doctor")
	for _, c := range coreDecodeRows[coreCheck](t, out) {
		if c.Name != "online:apple" {
			continue
		}
		if !c.OK {
			t.Fatalf("online check failed: %s", c.Detail)
		}
		if !strings.Contains(c.Detail, "calendar") {
			t.Errorf("detail = %q, want a calendar count", c.Detail)
		}
		return
	}
	t.Fatal("doctor ran no online check for the account")
}
