package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
	"github.com/teulaert/emlcalsync/internal/provider/oauth"
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
