package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
)

func coreDecodeRows[T any](t *testing.T, s string) []T {
	t.Helper()
	var rows []T
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &rows); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return rows
}

func coreDecodeOne[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	// A command may print a human line before the JSON; take the last line.
	lines := strings.Split(strings.TrimSpace(s), "\n")
	last := lines[len(lines)-1]
	if err := json.Unmarshal([]byte(last), &v); err != nil {
		t.Fatalf("decode %q: %v", last, err)
	}
	return v
}

func TestAccountList(t *testing.T) {
	env := newTestEnv(t)
	out := env.MustRun("account", "list")
	rows := coreDecodeRows[map[string]any](t, out)
	if len(rows) != 2 {
		t.Fatalf("account list returned %d rows: %s", len(rows), out)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r["name"].(string)] = r["provider"].(string)
	}
	if got["work"] != "fastmail" || got["home"] != "gmail" {
		t.Fatalf("account list = %v", got)
	}
}

func TestAccountAddFastmail(t *testing.T) {
	env := newTestEnv(t)
	// Register a provider for the account before adding it, so the online
	// verification at the end of `account add` succeeds.
	env.Mail["extra"] = fake.NewMail()
	env.Cal["extra"] = fake.NewCalendar()
	env.Stdin = "secret-token\n"

	out := env.MustRun("account", "add", "fastmail", "--name", "extra", "--email", "x@y.example", "--token-stdin")
	row := coreDecodeOne[map[string]any](t, out)
	if row["name"] != "extra" || row["provider"] != "fastmail" {
		t.Fatalf("add printed %v", row)
	}

	cfg, err := config.Load(env.Config)
	if err != nil {
		t.Fatal(err)
	}
	acct, ok := cfg.Account("extra")
	if !ok {
		t.Fatalf("config does not contain the new account: %+v", cfg.Accounts)
	}
	if !acct.Push || !acct.IncludeSpamTrash || acct.Poll.Duration() != config.DefaultPollFastmail {
		t.Errorf("defaults not applied: %+v", acct)
	}

	// The token landed in the secrets directory.
	sec, err := config.OpenSecrets(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := sec.Get(FastmailTokenKey(*acct))
	if err != nil {
		t.Fatalf("token not stored: %v", err)
	}
	if strings.TrimSpace(string(tok)) != "secret-token" {
		t.Errorf("stored token = %q", tok)
	}

	// Adding it twice is a usage error.
	if _, _, code := env.Run("account", "add", "fastmail", "--name", "extra", "--email", "x@y.example", "--token-stdin"); code != 2 {
		t.Errorf("duplicate add exit = %d, want 2", code)
	}
}

// A provider that cannot be reached must still leave the account in the config,
// so the user can fix the credentials and run `emlcal doctor`.
func TestAccountAddVerificationFails(t *testing.T) {
	env := newTestEnv(t)
	env.Stdin = "secret-token\n"
	_, errOut, code := env.Run("account", "add", "fastmail", "--name", "nofake", "--email", "x@y.example", "--token-stdin")
	if code != 5 {
		t.Fatalf("exit = %d, want 5 (provider); stderr: %s", code, errOut)
	}
	cfg, err := config.Load(env.Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Account("nofake"); !ok {
		t.Fatalf("account was rolled back on verification failure")
	}
}

func TestAccountAddBadName(t *testing.T) {
	env := newTestEnv(t)
	if _, _, code := env.Run("account", "add", "fastmail", "--name", "Not Valid", "--email", "x@y.example", "--token-stdin"); code != 2 {
		t.Errorf("bad name exit = %d, want 2", code)
	}
}

func TestAccountRemove(t *testing.T) {
	env := newTestEnv(t)
	env.Seed("work", fake.NewMsg("m1", RawMail(t, "a@example.com", "me@example.com", "Hi", "body", env.Now)))

	// Without --yes nothing is removed and the exit code is a usage error.
	out, _, code := env.Run("account", "remove", "work")
	if code != 2 {
		t.Fatalf("remove without --yes exit = %d, want 2", code)
	}
	if !strings.Contains(out, "work") {
		t.Errorf("remove without --yes did not describe the account: %s", out)
	}
	if cfg, err := config.Load(env.Config); err != nil || len(cfg.Accounts) != 2 {
		t.Fatalf("account was removed without --yes")
	}

	env.MustRun("account", "remove", "work", "--yes")
	cfg, err := config.Load(env.Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Account("work"); ok {
		t.Fatalf("account still configured: %+v", cfg.Accounts)
	}
	rows := coreDecodeRows[map[string]any](t, env.MustRun("account", "list"))
	if len(rows) != 1 || rows[0]["name"] != "home" {
		t.Fatalf("account list after remove = %v", rows)
	}
	if _, _, code := env.Run("account", "remove", "work", "--yes"); code != 3 {
		t.Errorf("removing a missing account exit = %d, want 3", code)
	}
}

func TestAccountGoogleClient(t *testing.T) {
	env := newTestEnv(t)
	env.MustRun("account", "google-client", "--id", "123456-abcdef.apps.googleusercontent.com", "--secret", "GOCSPX-GOCSPX-shh")
	cfg, err := config.Load(env.Config)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(cfg.SecretsDir() + "/" + GoogleClientKey)
	if err != nil {
		t.Fatalf("client credentials not stored: %v", err)
	}
	var v struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.ID != "123456-abcdef.apps.googleusercontent.com" || v.Secret != "GOCSPX-shh" {
		t.Errorf("stored %+v", v)
	}
}
