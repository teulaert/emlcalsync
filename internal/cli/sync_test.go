package cli

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
	"github.com/teulaert/emlcalsync/internal/provider/oauth"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// coreSeedSecrets gives every configured account the credential `doctor` looks
// for, without touching the network.
func coreSeedSecrets(t *testing.T, env *testEnv) {
	t.Helper()
	cfg, err := config.Load(env.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	sec, err := config.OpenSecrets(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range cfg.Accounts {
		switch a.Provider {
		case "fastmail":
			if err := sec.Set(FastmailTokenKey(a), []byte("token")); err != nil {
				t.Fatal(err)
			}
		case "gmail":
			tok := &oauth2.Token{AccessToken: "at", RefreshToken: "rt", Expiry: env.Now.Add(time.Hour)}
			if err := (oauth.FileTokenStore{Dir: cfg.SecretsDir()}).Save(GoogleTokenKey(a), tok); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestSyncBackfillThenDelta(t *testing.T) {
	env := newTestEnv(t)
	for i, subject := range []string{"one", "two", "three"} {
		env.Mail["work"].Add(fake.NewMsg(
			"m"+string(rune('1'+i)),
			RawMail(t, "a@example.com", "me@example.com", subject, "body", env.Now.Add(-time.Duration(i)*time.Hour)),
		))
	}

	rows := coreDecodeRows[coreSyncRow](t, env.MustRun("sync", "--account", "work"))
	var added int
	var kind string
	for _, r := range rows {
		if r.Resource == "mail" {
			added += r.Added
			kind = r.Kind
		}
	}
	if added != 3 {
		t.Fatalf("first sync added = %d, want 3 (rows %+v)", added, rows)
	}
	if kind != "backfill" {
		t.Errorf("first sync kind = %q, want backfill", kind)
	}

	rows = coreDecodeRows[coreSyncRow](t, env.MustRun("sync", "--account", "work"))
	for _, r := range rows {
		if r.Resource != "mail" {
			continue
		}
		if r.Added != 0 {
			t.Errorf("second sync added = %d, want 0", r.Added)
		}
		if r.Kind != "delta" {
			t.Errorf("second sync kind = %q, want delta", r.Kind)
		}
	}
}

func TestSyncUnknownAccount(t *testing.T) {
	env := newTestEnv(t)
	if _, _, code := env.Run("sync", "--account", "nope"); code != 3 {
		t.Fatalf("unknown account exit = %d, want 3", code)
	}
}

func TestStatus(t *testing.T) {
	env := newTestEnv(t)
	env.Seed("work",
		fake.NewMsg("m1", RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now.Add(-time.Hour))),
		fake.NewMsg("m2", RawMail(t, "b@example.com", "me@example.com", "two", "body", env.Now.Add(-2*time.Hour))),
	)

	out := coreDecodeOne[coreStatusOut](t, env.MustRun("status"))
	if len(out.Accounts) != 2 {
		t.Fatalf("status listed %d accounts", len(out.Accounts))
	}
	var work *coreStatusRow
	for i := range out.Accounts {
		if out.Accounts[i].Name == "work" {
			work = &out.Accounts[i]
		}
	}
	if work == nil {
		t.Fatal("no row for work")
	}
	if work.Messages != 2 {
		t.Errorf("work messages = %d, want 2", work.Messages)
	}
	if work.LastSync.IsZero() || work.LastSyncKind == "" {
		t.Errorf("work has no last sync: %+v", work)
	}
	if out.Daemon.Running {
		t.Errorf("daemon reported as running: %+v", out.Daemon)
	}
	if out.Blobs.Count != 2 {
		t.Errorf("blobs count = %d, want 2", out.Blobs.Count)
	}
	if out.DB == "" {
		t.Error("status did not report the database path")
	}
}

func TestDoctorPasses(t *testing.T) {
	env := newTestEnv(t)
	coreSeedSecrets(t, env)
	env.Seed("work", fake.NewMsg("m1", RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now)))

	out, errOut, code := env.Run("doctor")
	if code != 0 {
		t.Fatalf("doctor exit = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	checks := coreDecodeRows[coreCheck](t, out)
	if len(checks) == 0 {
		t.Fatal("doctor printed no checks")
	}
	seen := map[string]string{}
	for _, c := range checks {
		seen[c.Name] = c.Status
		if !c.OK {
			t.Errorf("check %s failed: %s", c.Name, c.Detail)
		}
	}
	for _, want := range []string{"config", "database", "blobs", "daemon", "secret:work", "online:work"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("no %q check in %v", want, seen)
		}
	}
}

// A missing credential is a hard failure, and doctor says so with exit 1.
func TestDoctorMissingSecret(t *testing.T) {
	env := newTestEnv(t)
	_, _, code := env.Run("doctor")
	if code != 1 {
		t.Fatalf("doctor exit = %d, want 1", code)
	}
}

// A live pid file means the daemon owns the sync loop; a second --watch must
// refuse instead of racing it for the lock.
func TestWatchRefusesSecondDaemon(t *testing.T) {
	env := newTestEnv(t)
	app, _, _ := env.App()
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// pid 1 is always alive and is never this process, so `sync --watch`
	// bails out before it starts the loop.
	pidPath := corePidPathOf(cfg)
	if err := os.WriteFile(pidPath, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app.Close()

	if pid, err := coreReadPid(app); err != nil || pid != 1 {
		t.Fatalf("coreReadPid = %d, %v", pid, err)
	}
	if !coreDaemonRunning(os.Getpid()) {
		t.Fatal("coreDaemonRunning says this process is not running")
	}
	if coreDaemonRunning(0) {
		t.Error("coreDaemonRunning(0) is true")
	}

	out, errOut, code := env.Run("sync", "--watch")
	if code != 1 {
		t.Fatalf("second --watch exit = %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}

	// `status` sees the same pid file.
	st := coreDecodeOne[coreStatusOut](t, env.MustRun("status"))
	if !st.Daemon.Running || st.Daemon.PID != 1 {
		t.Errorf("status daemon = %+v", st.Daemon)
	}
}

func TestStatusTableFooter(t *testing.T) {
	env := newTestEnv(t)
	env.Seed("work", fake.NewMsg("m1", RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now)))
	out := env.MustRun("status", "-o", "table")
	for _, want := range []string{"ACCOUNT", "daemon: not running", "blobs: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("table status does not contain %q:\n%s", want, out)
		}
	}
}

func TestSyncMutuallyExclusiveResourceFlags(t *testing.T) {
	env := newTestEnv(t)
	if _, _, code := env.Run("sync", "--mail-only", "--calendar-only"); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

// ---------------------------------------------------------------------------
// Riding out an outage

// With a budget, an outage that interrupts a backfill is a pause: the pass
// waits, resumes and finishes.
func TestSyncWaitOfflineRidesOutAnOutage(t *testing.T) {
	env := newTestEnv(t)
	for i := 1; i <= 3; i++ {
		env.Mail["work"].Add(fake.NewMsg(fmt.Sprintf("m%d", i),
			RawMail(t, "a@example.com", "me@example.com", fmt.Sprintf("s%d", i), "body", env.Now)))
	}
	env.Mail["work"].SetPageSize(1)
	// The first page lands, the second finds no network.
	env.Mail["work"].OnEnumerate(func(call int) {
		if call == 2 {
			env.Mail["work"].FailNext(1)
		}
	})

	out, errOut, code := env.Run("sync", "--account", "work", "--wait-offline", "2s")
	if code != 0 {
		t.Fatalf("sync exit = %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	rows := coreDecodeRows[coreSyncRow](t, out)
	var added int
	for _, r := range rows {
		if r.Resource == "mail" {
			added += r.Added
		}
	}
	if added != 3 {
		t.Errorf("added = %d, want 3 (rows %+v)", added, rows)
	}
}

// No network at all is not a pause: DESIGN.md §12 promises exit 4 straight
// away, so a generous budget must not make `emlcal sync` block its caller.
func TestSyncWithNothingInFlightExitsFourImmediately(t *testing.T) {
	env := newTestEnv(t)
	env.Mail["work"].Add(fake.NewMsg("m1",
		RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now)))
	env.Mail["work"].FailNext(1000)

	started := time.Now()
	_, _, code := env.Run("sync", "--account", "work", "--wait-offline", "10m")
	if code != 4 {
		t.Fatalf("sync exit = %d, want 4", code)
	}
	if took := time.Since(started); took > 30*time.Second {
		t.Fatalf("took %s; an outage with nothing in flight must not be waited out", took)
	}
}

// --wait-offline 0 is the documented escape hatch: exit 4 straight away.
func TestSyncWaitOfflineZeroExitsFour(t *testing.T) {
	env := newTestEnv(t)
	env.Mail["work"].Add(fake.NewMsg("m1",
		RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now)))
	env.Mail["work"].FailNext(20)

	_, _, code := env.Run("sync", "--account", "work", "--wait-offline", "0")
	if code != 4 {
		t.Fatalf("sync exit = %d, want 4", code)
	}
}

func TestSyncRejectsNegativeWaitOffline(t *testing.T) {
	env := newTestEnv(t)
	if _, _, code := env.Run("sync", "--wait-offline", "-1m"); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

// ---------------------------------------------------------------------------
// Progress line

func TestSyncQuietSuppressesTheProgressLine(t *testing.T) {
	env := newTestEnv(t)
	for i, subject := range []string{"one", "two", "three"} {
		env.Mail["work"].Add(fake.NewMsg(fmt.Sprintf("m%d", i),
			RawMail(t, "a@example.com", "me@example.com", subject, "body", env.Now)))
	}

	// A TTY without --quiet draws the live line on stderr...
	app, _, errb := env.App()
	app.IsTTY = true
	if code := Execute([]string{"sync", "--account", "work"}, app); code != 0 {
		t.Fatalf("sync exit = %d: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "work mail") {
		t.Errorf("no progress line on a TTY:\n%q", errb.String())
	}

	// ...and --quiet leaves stderr alone.
	env2 := newTestEnv(t)
	env2.Mail["work"].Add(fake.NewMsg("m1",
		RawMail(t, "a@example.com", "me@example.com", "one", "body", env2.Now)))
	app2, _, errb2 := env2.App()
	app2.IsTTY = true
	if code := Execute([]string{"sync", "--account", "work", "--quiet"}, app2); code != 0 {
		t.Fatalf("quiet sync exit = %d: %s", code, errb2.String())
	}
	if strings.Contains(errb2.String(), "work mail") {
		t.Errorf("--quiet still printed progress:\n%q", errb2.String())
	}
}

// The line shows what the engine composed (counts, rate, ETA), and each event
// overwrites the previous one.
func TestProgressPrinterRendersTheEngineMessage(t *testing.T) {
	app, _, errb := (&testEnv{}).appForProgress()
	p := coreProgressPrinter(app)
	p(sync.ProgressEvent{Account: "work", Resource: "mail", Phase: "backfill",
		Done: 1234, Total: 52000, Message: "1 234/52 000 · 48/s · ~18m"})
	p(sync.ProgressEvent{Account: "work", Resource: "mail", Phase: "offline",
		Message: "waiting for network (retry in 30s, 8m left)"})

	got := errb.String()
	for _, want := range []string{
		"work mail backfill 1 234/52 000 · 48/s · ~18m",
		"work mail offline waiting for network (retry in 30s, 8m left)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("progress output does not contain %q:\n%q", want, got)
		}
	}
	if !strings.Contains(got, "\r") {
		t.Error("progress line does not overwrite itself")
	}
}

// appForProgress is a bare App writing to buffers, for the printer test.
func (e *testEnv) appForProgress() (*App, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &App{Stdout: &out, Stderr: &errb, IsTTY: true}, &out, &errb
}

// ---------------------------------------------------------------------------
// Per-account toggles

func TestSyncSkipsDisabledResources(t *testing.T) {
	env := newTestEnv(t, config.Account{
		Name: "work", Provider: model.ProviderFastmail, Email: "me@example.com",
		Mail: false, Calendar: true, Calendars: []string{"*"},
	})
	env.Mail["work"].Add(fake.NewMsg("m1",
		RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now)))

	rows := coreDecodeRows[coreSyncRow](t, env.MustRun("sync"))
	var mail *coreSyncRow
	for i := range rows {
		if rows[i].Resource == "mail" {
			mail = &rows[i]
		}
	}
	if mail == nil || mail.Kind != "disabled" {
		t.Fatalf("mail row = %+v, want kind disabled", mail)
	}

	st := coreDecodeOne[coreStatusOut](t, env.MustRun("status"))
	if len(st.Accounts) != 1 {
		t.Fatalf("status listed %d accounts", len(st.Accounts))
	}
	if st.Accounts[0].Mail || !st.Accounts[0].Calendar {
		t.Errorf("status toggles = mail:%v calendar:%v", st.Accounts[0].Mail, st.Accounts[0].Calendar)
	}
	if st.Accounts[0].Disabled != "mail off" {
		t.Errorf("status disabled = %q, want %q", st.Accounts[0].Disabled, "mail off")
	}
	if st.Accounts[0].Messages != 0 {
		t.Errorf("indexed %d messages for a mail-disabled account", st.Accounts[0].Messages)
	}
}

// ---------------------------------------------------------------------------
// Unknown commands

func TestUnknownCommandNamesTheCommandNotTheFlag(t *testing.T) {
	env := newTestEnv(t)
	out, errOut, code := env.Run("add", "fastmail", "--name", "x")
	if code != 2 {
		t.Fatalf("exit = %d, want 2\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(errOut, `unknown command \"add\" for \"emlcal\"`) &&
		!strings.Contains(errOut, `unknown command "add" for "emlcal"`) {
		t.Errorf("stderr does not name the command:\n%s", errOut)
	}
	if !strings.Contains(errOut, "emlcal account add") {
		t.Errorf("stderr does not suggest the real command:\n%s", errOut)
	}
}

func TestUnknownCommandWithoutAHint(t *testing.T) {
	env := newTestEnv(t)
	_, errOut, code := env.Run("zzzz")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "zzzz") {
		t.Errorf("stderr does not name the word:\n%s", errOut)
	}
	if strings.Contains(errOut, "did you mean") {
		t.Errorf("invented a suggestion for a word nothing matches:\n%s", errOut)
	}
}

// Real commands, their flags and the built-ins still run.
func TestKnownCommandsAreNotMistakenForUnknownOnes(t *testing.T) {
	env := newTestEnv(t)
	for _, args := range [][]string{
		{"--help"},
		{"help", "sync"},
		{"status"},
		{"-o", "json", "status"},
		{"--account", "work", "status"},
		{"completion", "bash"},
		{"sync", "--account", "work", "--quiet"},
		// cobra registers its completion machinery during Execute, after the
		// unknown-command check has already looked at the first word.
		{"__complete", "sy", ""},
	} {
		out, errOut, code := env.Run(args...)
		if code != 0 {
			t.Errorf("emlcal %s: exit %d\nstdout: %s\nstderr: %s",
				strings.Join(args, " "), code, out, errOut)
		}
	}
}

// A healthy run used to leave an empty log file, because the engine only
// logged at Warn. Every pass now records what it did.
func TestSyncLeavesInfoLinesInTheLog(t *testing.T) {
	env := newTestEnv(t)
	env.Mail["work"].Add(fake.NewMsg("m1",
		RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now)))
	env.MustRun("sync", "--account", "work")

	cfg, err := config.Load(env.Config)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(cfg.LogPath())
	if err != nil {
		t.Fatalf("read %s: %v", cfg.LogPath(), err)
	}
	log := string(b)
	for _, want := range []string{
		`level=INFO`,
		`msg="sync starting" account=work resource=mail`,
		`msg="sync finished" account=work resource=mail kind=backfill added=1`,
		`msg="mail backfill" account=work`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log does not contain %q:\n%s", want, log)
		}
	}
}
