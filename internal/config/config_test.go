package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

// exampleTOML is the configuration printed in DESIGN.md §11, verbatim.
const exampleTOML = `# ~/.config/emlcal/config.toml
[general]
timezone       = "Europe/Amsterdam"     # default: system
default_format = "auto"                 # auto | json | table
raw_max_size   = "0"                    # global default, per-account override

[[accounts]]
name     = "work"
provider = "gmail"
email    = "lennert@example.com"
poll     = "60s"
include_spam_trash = true

[[accounts]]
name     = "personal"
provider = "fastmail"
email    = "lennert@fastmail.example"
push     = true
calendars = ["*"]                       # or explicit list of names
`

// writeTemp puts contents in a fresh directory and returns the file path.
func writeTemp(t *testing.T, name, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDesignExample(t *testing.T) {
	c, err := Load(writeTemp(t, "config.toml", exampleTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if c.General.Timezone != "Europe/Amsterdam" {
		t.Errorf("timezone = %q", c.General.Timezone)
	}
	if c.General.DefaultFormat != "auto" {
		t.Errorf("default_format = %q", c.General.DefaultFormat)
	}
	if c.General.RawMaxSize != Unlimited {
		t.Errorf("raw_max_size = %v, want unlimited", c.General.RawMaxSize)
	}
	if c.General.SecretBackend != BackendFile {
		t.Errorf("secret_backend = %q, want the file default", c.General.SecretBackend)
	}
	if len(c.Accounts) != 2 {
		t.Fatalf("got %d accounts, want 2", len(c.Accounts))
	}

	work, ok := c.Account("work")
	if !ok {
		t.Fatal("account work missing")
	}
	if work.Provider != model.ProviderGmail || work.Email != "lennert@example.com" {
		t.Errorf("work = %+v", work)
	}
	if work.Poll.Duration() != 60*time.Second {
		t.Errorf("work poll = %v", work.Poll)
	}
	if !work.IncludeSpamTrash {
		t.Error("work include_spam_trash should be true")
	}
	if work.Push {
		t.Error("gmail has no push stream; push should default to false")
	}
	if !work.SyncsAllCalendars() {
		t.Error("calendars should default to the wildcard")
	}

	personal, ok := c.Account("personal")
	if !ok {
		t.Fatal("account personal missing")
	}
	if personal.Provider != model.ProviderFastmail {
		t.Errorf("personal provider = %q", personal.Provider)
	}
	if !personal.Push {
		t.Error("fastmail push should be true")
	}
	if personal.Poll.Duration() != DefaultPollFastmail {
		t.Errorf("personal poll = %v, want the fastmail fallback %v", personal.Poll, DefaultPollFastmail)
	}
	if !personal.IncludeSpamTrash {
		t.Error("include_spam_trash should default to true")
	}
	if personal.Concurrency != DefaultConcurrency {
		t.Errorf("concurrency = %d", personal.Concurrency)
	}
	if _, ok := c.Account("nope"); ok {
		t.Error("Account returned an unknown name")
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Accounts) != 0 {
		t.Errorf("got %d accounts, want none", len(c.Accounts))
	}
	if c.General.DefaultFormat != DefaultFormat || c.General.SecretBackend != DefaultSecretBackend {
		t.Errorf("defaults not applied: %+v", c.General)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("a fresh install must validate: %v", err)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	p := writeTemp(t, "config.toml", "[general]\ntimezoen = \"UTC\"\n")
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "timezoen") {
		t.Fatalf("want an error naming the typo, got %v", err)
	}
}

func TestPerAccountOverrides(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[general]
raw_max_size = "25MB"

[[accounts]]
name = "work"
provider = "gmail"
email = "a@b.example"
poll = "5m"
push = false
include_spam_trash = false
raw_max_size = "0"
calendars = ["Work", "Family"]
concurrency = 8
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	a := &c.Accounts[0]
	if a.Poll.Duration() != 5*time.Minute {
		t.Errorf("poll = %v", a.Poll)
	}
	if a.IncludeSpamTrash {
		t.Error("include_spam_trash = false should stick, not fall back to the default")
	}
	if a.Concurrency != 8 {
		t.Errorf("concurrency = %d", a.Concurrency)
	}
	if got := a.EffectiveRawMaxSize(c.General); got != Unlimited {
		t.Errorf("account override lost: got %v, want unlimited", got)
	}
	if a.SyncsAllCalendars() {
		t.Error("explicit calendar list should not be the wildcard")
	}
	if !a.WantsCalendar("work") || a.WantsCalendar("Other") {
		t.Error("WantsCalendar should match case-insensitively and only listed names")
	}

	// Without an override the account inherits the general cap.
	b := Account{}
	if got := b.EffectiveRawMaxSize(c.General); got != 25_000_000 {
		t.Errorf("inherited raw_max_size = %v", got)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		cfg  func(*Config)
		want string
	}{
		{"duplicate names", func(c *Config) {
			c.Accounts = append(c.Accounts, c.Accounts[0])
		}, "duplicate"},
		{"bad account id", func(c *Config) {
			c.Accounts[0].Name = "Work Account"
		}, "[a-z0-9-]"},
		{"unknown provider", func(c *Config) {
			c.Accounts[0].Provider = "outlook"
		}, "unknown provider"},
		{"missing email", func(c *Config) {
			c.Accounts[0].Email = ""
		}, "email is required"},
		{"bad format", func(c *Config) {
			c.General.DefaultFormat = "yaml"
		}, "default_format"},
		{"bad backend", func(c *Config) {
			c.General.SecretBackend = "1password"
		}, "secret_backend"},
		{"bad timezone", func(c *Config) {
			c.General.Timezone = "Mars/Olympus"
		}, "timezone"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.Accounts = []Account{{
				Name: "work", Provider: model.ProviderGmail, Email: "a@b.example",
			}}
			tc.cfg(c)
			err := c.Validate()
			if err == nil {
				t.Fatal("want an error")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error should wrap ErrInvalid: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want Size
	}{
		{"0", 0},
		{"512", 512},
		{"512B", 512},
		{"25MB", 25_000_000},
		{"25mb", 25_000_000},
		{" 1GiB ", 1 << 30},
		{"1KiB", 1024},
		{"2.5MB", 2_500_000},
		{"1M", 1_000_000},
	}
	for _, tc := range tests {
		got, err := ParseSize(tc.in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "MB", "-1", "12 apples", "1EB"} {
		if v, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", bad, v)
		}
	}
}

func TestSizeStringRoundTrip(t *testing.T) {
	for _, s := range []Size{0, 512, 1024, 25_000_000, 1 << 30} {
		got, err := ParseSize(s.String())
		if err != nil {
			t.Fatalf("ParseSize(%q): %v", s.String(), err)
		}
		if got != s {
			t.Errorf("%d -> %q -> %d", s, s.String(), got)
		}
	}
}

func TestParseDuration(t *testing.T) {
	d, err := ParseDuration("90s")
	if err != nil || d.Duration() != 90*time.Second {
		t.Fatalf("ParseDuration(90s) = %v, %v", d, err)
	}
	if _, err := ParseDuration("-5s"); err == nil {
		t.Error("negative durations should be rejected")
	}
	if _, err := ParseDuration("soon"); err == nil {
		t.Error("nonsense durations should be rejected")
	}
}

func TestXDGResolution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	if got, want := ConfigDir(), filepath.Join(home, ".config", "emlcal"); got != want {
		t.Errorf("ConfigDir = %q, want %q", got, want)
	}
	if got, want := DataDir(), filepath.Join(home, ".local", "share", "emlcal"); got != want {
		t.Errorf("DataDir = %q, want %q", got, want)
	}
	if got, want := StateDir(), filepath.Join(home, ".local", "state", "emlcal"); got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}

	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got, want := ConfigDir(), filepath.Join(xdg, "emlcal"); got != want {
		t.Errorf("ConfigDir with XDG_CONFIG_HOME = %q, want %q", got, want)
	}
	// A relative XDG value is ignored per the spec.
	t.Setenv("XDG_CONFIG_HOME", "relative/path")
	if got, want := ConfigDir(), filepath.Join(home, ".config", "emlcal"); got != want {
		t.Errorf("relative XDG_CONFIG_HOME should be ignored: got %q", got)
	}
}

func TestPaths(t *testing.T) {
	c := Default()
	c.General.DataDir = "/data"
	c.General.ConfigDir = "/conf"
	c.General.StateDir = "/state"

	db, blobs, secrets, lock, log := c.Paths()
	if db != "/data/emlcal.db" {
		t.Errorf("db = %q", db)
	}
	if blobs != "/data/blobs" {
		t.Errorf("blobs = %q", blobs)
	}
	if secrets != "/conf/secrets" {
		t.Errorf("secrets = %q", secrets)
	}
	if lock != "/state/emlcal.lock" {
		t.Errorf("lock = %q", lock)
	}
	if log != "/state/emlcal.log" {
		t.Errorf("log = %q", log)
	}
	if got := c.LockPath("work"); got != "/state/sync.work.lock" {
		t.Errorf("LockPath = %q", got)
	}
}

func TestEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvFormat, "json")

	c, err := Load(writeTemp(t, "config.toml", exampleTOML))
	if err != nil {
		t.Fatal(err)
	}
	if c.General.DataDir != dir {
		t.Errorf("EMLCAL_DATA_DIR ignored: %q", c.General.DataDir)
	}
	if c.General.DefaultFormat != "json" {
		t.Errorf("EMLCAL_FORMAT ignored: %q", c.General.DefaultFormat)
	}

	// EMLCAL_CONFIG steers DefaultPath.
	t.Setenv(EnvConfig, "/somewhere/else.toml")
	if got := DefaultPath(); got != "/somewhere/else.toml" {
		t.Errorf("DefaultPath = %q", got)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	max := Size(25_000_000)
	orig := Default()
	orig.General.Timezone = "Europe/Amsterdam"
	orig.General.RawMaxSize = 1 << 30
	orig.General.DataDir = filepath.Join(dir, "data")
	orig.Accounts = []Account{
		{
			Name: "work", Provider: model.ProviderGmail, Email: "lennert@example.com",
			Poll: Duration(90 * time.Second), Push: false, IncludeSpamTrash: false,
			RawMaxSize: &max, Calendars: []string{"Work"}, Concurrency: 8,
		},
		{
			Name: "personal", Provider: model.ProviderFastmail, Email: "l@fastmail.example",
			Poll: Duration(DefaultPollFastmail), Push: true, IncludeSpamTrash: true,
			Calendars: []string{"*"}, Concurrency: DefaultConcurrency,
		},
	}

	p := filepath.Join(dir, "config.toml")
	if err := Save(p, orig); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode = %o, want 600", perm)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "#") {
		t.Error("saved config should carry explanatory comments")
	}

	back, err := Load(p)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("round-tripped config does not validate: %v", err)
	}
	if back.General.Timezone != orig.General.Timezone ||
		back.General.RawMaxSize != orig.General.RawMaxSize ||
		back.General.DataDir != orig.General.DataDir {
		t.Errorf("general changed: %+v vs %+v", back.General, orig.General)
	}
	if len(back.Accounts) != len(orig.Accounts) {
		t.Fatalf("got %d accounts, want %d", len(back.Accounts), len(orig.Accounts))
	}
	for i := range orig.Accounts {
		a, b := orig.Accounts[i], back.Accounts[i]
		if a.Name != b.Name || a.Provider != b.Provider || a.Email != b.Email ||
			a.Poll != b.Poll || a.Push != b.Push || a.IncludeSpamTrash != b.IncludeSpamTrash ||
			a.Concurrency != b.Concurrency || !equalStrings(a.Calendars, b.Calendars) {
			t.Errorf("account %d changed:\n orig %+v\n back %+v", i, a, b)
		}
		switch {
		case (a.RawMaxSize == nil) != (b.RawMaxSize == nil):
			t.Errorf("account %d raw_max_size presence changed", i)
		case a.RawMaxSize != nil && *a.RawMaxSize != *b.RawMaxSize:
			t.Errorf("account %d raw_max_size = %v, want %v", i, *b.RawMaxSize, *a.RawMaxSize)
		}
	}
}

func TestSaveEscapesStrings(t *testing.T) {
	c := Default()
	c.Accounts = []Account{{
		Name: "work", Provider: model.ProviderGmail, Email: `a"b@example.com`,
		Poll: Duration(DefaultPollGmail), IncludeSpamTrash: true,
		Calendars: []string{`Odd "name"`}, Concurrency: DefaultConcurrency,
	}}
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	back, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.Accounts[0].Email != `a"b@example.com` {
		t.Errorf("email = %q", back.Accounts[0].Email)
	}
	if back.Accounts[0].Calendars[0] != `Odd "name"` {
		t.Errorf("calendar = %q", back.Accounts[0].Calendars[0])
	}
}

func TestFileSecrets(t *testing.T) {
	c := Default()
	c.General.ConfigDir = t.TempDir()

	s, err := OpenSecrets(c)
	if err != nil {
		t.Fatalf("OpenSecrets: %v", err)
	}

	fi, err := os.Stat(c.SecretsDir())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("secrets dir mode = %o, want 700", perm)
	}

	key := (&Account{Name: "work", Provider: model.ProviderGmail}).SecretKey()
	if key != "work.gmail.json" {
		t.Errorf("SecretKey = %q", key)
	}

	if _, err := s.Get(key); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("missing secret should wrap model.ErrNotFound, got %v", err)
	}
	if err := s.Set(key, []byte(`{"token":"abc"}`)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	fi, err = os.Stat(filepath.Join(c.SecretsDir(), key))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("secret mode = %o, want 600", perm)
	}
	got, err := s.Get(key)
	if err != nil || string(got) != `{"token":"abc"}` {
		t.Fatalf("Get = %q, %v", got, err)
	}

	// Overwrite must replace, not append.
	if err := s.Set(key, []byte("second")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(key); string(got) != "second" {
		t.Errorf("after overwrite Get = %q", got)
	}

	if keys, err := s.(Lister).List(); err != nil || len(keys) != 1 || keys[0] != key {
		t.Errorf("List = %v, %v", keys, err)
	}
	if err := s.Delete(key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(key); err != nil {
		t.Errorf("deleting a missing secret should be a no-op, got %v", err)
	}
	if _, err := s.Get(key); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("after Delete, Get = %v", err)
	}
}

func TestFileSecretsRejectsPathTraversal(t *testing.T) {
	s := &FileSecrets{Dir: t.TempDir()}
	for _, key := range []string{"../escape", "sub/dir", "", ".", "..", ".hidden"} {
		if err := s.Set(key, []byte("x")); !errors.Is(err, ErrBadKey) {
			t.Errorf("Set(%q) = %v, want ErrBadKey", key, err)
		}
	}
}

func TestOpenSecretsLibsecretNotImplemented(t *testing.T) {
	c := Default()
	c.General.SecretBackend = BackendLibsecret
	_, err := OpenSecrets(c)
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("want a clear not-implemented error, got %v", err)
	}
}

func TestLocation(t *testing.T) {
	c := Default()
	if loc, err := c.Location(); err != nil || loc != time.Local {
		t.Errorf("empty timezone should give the system zone: %v, %v", loc, err)
	}
	c.General.Timezone = "Europe/Amsterdam"
	loc, err := c.Location()
	if err != nil || loc.String() != "Europe/Amsterdam" {
		t.Errorf("Location = %v, %v", loc, err)
	}
}

func TestEnsureDirs(t *testing.T) {
	dir := t.TempDir()
	c := Default()
	c.General.DataDir = filepath.Join(dir, "data")
	c.General.StateDir = filepath.Join(dir, "state")
	if err := c.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{c.General.DataDir, c.General.StateDir, c.BlobsDir()} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode = %o, want 700", p, perm)
		}
	}
}

func TestDefaultAccount(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[general]
default_account = "personal"

[[accounts]]
name = "work"
provider = "gmail"
email = "a@b.example"

[[accounts]]
name = "personal"
provider = "fastmail"
email = "c@d.example"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.General.DefaultAccount != "personal" {
		t.Errorf("default_account = %q", c.General.DefaultAccount)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// It must name a configured account.
	c.General.DefaultAccount = "nobody"
	err = c.Validate()
	if err == nil || !strings.Contains(err.Error(), "default_account") {
		t.Errorf("an unknown default_account should be rejected, got %v", err)
	}

	// Absent is fine: there is simply no default.
	c.General.DefaultAccount = ""
	if err := c.Validate(); err != nil {
		t.Errorf("an empty default_account should be allowed: %v", err)
	}
}

func TestDefaultAccountSurvivesSave(t *testing.T) {
	c := Default()
	c.General.DefaultAccount = "work"
	c.Accounts = []Account{{
		Name: "work", Provider: model.ProviderGmail, Email: "a@b.example",
		Poll: Duration(DefaultPollGmail), IncludeSpamTrash: true,
		Calendars: []string{"*"}, Concurrency: DefaultConcurrency,
	}}
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	back, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if back.General.DefaultAccount != "work" {
		t.Errorf("default_account = %q after round trip", back.General.DefaultAccount)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestIncludeSpamTrashDefaultsTrue(t *testing.T) {
	// Neither provider opts out by default: Junk and Trash are part of the
	// archive unless the account explicitly says otherwise.
	p := writeTemp(t, "config.toml", `
[[accounts]]
name = "work"
provider = "gmail"
email = "a@b.example"

[[accounts]]
name = "personal"
provider = "fastmail"
email = "c@d.example"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := range c.Accounts {
		if !c.Accounts[i].IncludeSpamTrash {
			t.Errorf("account %q: include_spam_trash should default to true", c.Accounts[i].Name)
		}
	}
}
