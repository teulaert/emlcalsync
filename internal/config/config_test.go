package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
email    = "lennert@example.com"
poll     = "60s"
include_spam_trash = true

  [accounts.mail]
  backend = "gmail"

  [accounts.calendar]
  backend = "gcal"

[[accounts]]
name     = "personal"
email    = "lennert@fastmail.example"
push     = true
calendars = ["*"]                       # or explicit list of names

  [accounts.mail]
  backend = "jmap"

  [accounts.calendar]
  backend = "caldav"
  vendor  = "fastmail"

# Language models the TUI can draft with (ctrl+g in the composer). Absent =
# off. Only Ollama for now; the block is shaped for more.
[ai]
default = "local"

[[ai.models]]
name    = "local"
backend = "ollama"                      # default
model   = "qwen3:32b"
url     = "http://localhost:11434"      # default
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
	if m, ok := c.DefaultAIModel(); !ok || m.Name != "local" || m.Model != "qwen3:32b" {
		t.Errorf("ai model = %+v, %v", m, ok)
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
	if work.Vendor() != model.VendorGoogle || work.Email != "lennert@example.com" {
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
	if personal.Vendor() != model.VendorFastmail {
		t.Errorf("personal vendor = %q", personal.Vendor())
	}
	if personal.Calendar == nil || personal.Calendar.Backend != model.BackendCalDAV {
		t.Errorf("personal calendar = %+v, want a caldav backend", personal.Calendar)
	}
	if !personal.Push {
		t.Error("fastmail push should be true")
	}
	if personal.Poll.Duration() != DefaultPollJMAP {
		t.Errorf("personal poll = %v, want the fastmail fallback %v", personal.Poll, DefaultPollJMAP)
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
email = "a@b.example"
poll = "5m"
push = false
include_spam_trash = false
raw_max_size = "0"
calendars = ["Work", "Family"]
concurrency = 8

  [accounts.mail]
  backend = "gmail"

  [accounts.calendar]
  backend = "gcal"

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
		{"unknown mail backend", func(c *Config) {
			c.Accounts[0].Mail = &MailBackend{Backend: "outlook"}
		}, "unknown mail backend"},
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
				Name: "work", Email: "a@b.example",
				Mail: &MailBackend{Backend: model.BackendGmail, Vendor: model.VendorGoogle},
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
			Name: "work", Email: "lennert@example.com",
			Mail:     &MailBackend{Backend: model.BackendGmail, Vendor: model.VendorGoogle},
			Calendar: &CalendarBackend{Backend: model.BackendGCal, Vendor: model.VendorGoogle},
			Poll:     Duration(90 * time.Second), Push: false, IncludeSpamTrash: false,
			RawMaxSize: &max, Calendars: []string{"Work"}, Concurrency: 8,
		},
		{
			Name: "personal", Email: "l@fastmail.example",
			Mail:     &MailBackend{Backend: model.BackendJMAP, Vendor: model.VendorFastmail},
			Calendar: &CalendarBackend{Backend: model.BackendCalDAV, Vendor: model.VendorFastmail},
			Poll:     Duration(DefaultPollJMAP), Push: true, IncludeSpamTrash: true,
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
		if a.Name != b.Name || a.Vendor() != b.Vendor() || a.Email != b.Email ||
			!sameMail(a.Mail, b.Mail) || !sameCalendar(a.Calendar, b.Calendar) ||
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
		Name: "work", Email: `a"b@example.com`,
		Mail:     &MailBackend{Backend: model.BackendGmail, Vendor: model.VendorGoogle},
		Calendar: &CalendarBackend{Backend: model.BackendGCal, Vendor: model.VendorGoogle},
		Poll:     Duration(DefaultPollGmail), IncludeSpamTrash: true,
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

	key := "work.google.json"
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
	// The system zone must come back under its IANA name, never as "Local"
	// (Google rejects that; see systemZone).
	t.Setenv("TZ", "Europe/Brussels")
	if loc, err := c.Location(); err != nil || loc.String() != "Europe/Brussels" {
		t.Errorf("empty timezone should resolve the system zone by name: %v, %v", loc, err)
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
email = "a@b.example"

  [accounts.mail]
  backend = "gmail"

  [accounts.calendar]
  backend = "gcal"

[[accounts]]
name = "personal"
email = "c@d.example"

  [accounts.mail]
  backend = "jmap"

  [accounts.calendar]
  backend = "caldav"
  vendor  = "fastmail"

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
		Name: "work", Email: "a@b.example",
		Mail: &MailBackend{Backend: model.BackendGmail, Vendor: model.VendorGoogle},
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
email = "a@b.example"

  [accounts.mail]
  backend = "gmail"

  [accounts.calendar]
  backend = "gcal"

[[accounts]]
name = "personal"
email = "c@d.example"

  [accounts.mail]
  backend = "jmap"

  [accounts.calendar]
  backend = "caldav"
  vendor  = "fastmail"

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

// ---------------------------------------------------------------------------
// Per-account mail/calendar toggles

// An absent key means the half is on: only an explicit false switches it off.
// A resource block's presence is what switches that resource on; there are no
// mail/calendar booleans to disagree with it.
func TestAccountResourcesFollowTheirBlocks(t *testing.T) {
	path := writeTemp(t, "config.toml", `

[[accounts]]
name = "work"
email = "me@work.example"

  [accounts.mail]
  backend = "gmail"

  [accounts.calendar]
  backend = "gcal"

[[accounts]]
name = "cal-only"
email = "me@corp.example"

  [accounts.calendar]
  backend = "gcal"

[[accounts]]
name = "mail-only"
email = "me@fastmail.example"

  [accounts.mail]
  backend = "jmap"

`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	for _, tc := range []struct {
		name           string
		mail, calendar bool
	}{
		{"work", true, true},
		{"cal-only", false, true},
		{"mail-only", true, false},
	} {
		a, ok := c.Account(tc.name)
		if !ok {
			t.Fatalf("no account %q", tc.name)
		}
		if a.SyncsMail() != tc.mail || a.SyncsCalendar() != tc.calendar {
			t.Errorf("%s: mail=%v calendar=%v, want mail=%v calendar=%v",
				tc.name, a.SyncsMail(), a.SyncsCalendar(), tc.mail, tc.calendar)
		}
		if a.Syncs("mail") != tc.mail || a.Syncs("calendar") != tc.calendar {
			t.Errorf("%s: Syncs disagrees with the fields", tc.name)
		}
		if a.Syncs("contacts") {
			t.Errorf("%s: Syncs said yes to a resource that does not exist", tc.name)
		}
	}
}

// An account with both halves off would sync nothing, which is a mistake worth
// naming rather than a configuration to honour.
func TestValidateRejectsAnAccountWithNoResourceBlocks(t *testing.T) {
	path := writeTemp(t, "config.toml", `

[[accounts]]
name = "work"
email = "me@work.example"

`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = c.Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error should wrap ErrInvalid: %v", err)
	}
	if !strings.Contains(err.Error(), "would sync nothing") {
		t.Errorf("error %q does not explain the problem", err)
	}
}

// A block is written iff the account has one, and an account built in Go with
// no blocks at all must not become a file Load then refuses.
func TestSaveRoundTripsResourceBlocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	c := Default()
	c.Accounts = []Account{
		{Name: "cal-only", Email: "a@b.example",
			Calendar:  &CalendarBackend{Backend: model.BackendGCal, Vendor: model.VendorGoogle},
			Calendars: []string{"*"}, Concurrency: DefaultConcurrency},
		NewAccount("both", "c@d.example", model.VendorGoogle),
	}
	if err := Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "\n  [accounts.calendar]") != 2 {
		t.Errorf("want a calendar block on both accounts:\n%s", b)
	}
	if strings.Count(string(b), "\n  [accounts.mail]") != 1 {
		t.Errorf("cal-only must not get a mail block:\n%s", b)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate after round trip: %v", err)
	}
	a, _ := got.Account("cal-only")
	if a.SyncsMail() || !a.SyncsCalendar() {
		t.Errorf("cal-only came back as mail=%v calendar=%v", a.SyncsMail(), a.SyncsCalendar())
	}
	l, _ := got.Account("both")
	if !l.SyncsMail() || !l.SyncsCalendar() {
		t.Errorf("both came back as mail=%v calendar=%v, want both on", l.SyncsMail(), l.SyncsCalendar())
	}
}

// sameMail and sameCalendar compare two backend blocks including nil-ness, so
// a round trip that drops or invents a block is caught.
func sameMail(a, b *MailBackend) bool {
	if a == nil || b == nil {
		return a == b
	}
	// reflect.DeepEqual rather than ==: the block carries folder lists now.
	return reflect.DeepEqual(*a, *b)
}

func sameCalendar(a, b *CalendarBackend) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ---------------------------------------------------------------------------
// IMAP

func TestLoadIMAPAccount(t *testing.T) {
	c, err := Load(writeTemp(t, "config.toml", `
[[accounts]]
name  = "apple"
email = "me@icloud.example"

  [accounts.mail]
  backend  = "imap"
  vendor   = "icloud"
  username = "me@example.com"

  [accounts.calendar]
  backend = "caldav"
  vendor  = "icloud"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	a, ok := c.Account("apple")
	if !ok {
		t.Fatal("account missing")
	}
	if !a.SyncsMail() {
		t.Fatal("an iCloud account should sync mail now")
	}
	if a.Mail.Backend != model.BackendIMAP || a.Mail.Vendor != model.VendorICloud {
		t.Errorf("mail block = %+v", a.Mail)
	}
	if a.Mail.User(a.Email) != "me@example.com" {
		t.Errorf("login = %q, want the Apple ID", a.Mail.User(a.Email))
	}
	if a.Mail.SMTPUser(a.Email) != "me@example.com" {
		t.Errorf("smtp login = %q, want the IMAP one", a.Mail.SMTPUser(a.Email))
	}
	if !a.Push {
		t.Error("IMAP has IDLE, so push should default on")
	}
}

func TestLoadSelfHostedIMAPNeedsNoVendor(t *testing.T) {
	c, err := Load(writeTemp(t, "config.toml", `
[[accounts]]
name  = "home"
email = "me@example.com"

  [accounts.mail]
  backend       = "imap"
  host          = "mail.example.com"
  port          = 143
  security      = "starttls"
  smtp_host     = "mail.example.com"
  smtp_port     = 587
  archive_folder = "Archief"
  exclude_folders = ["Junk"]
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	a, _ := c.Account("home")
	if a.Mail.Host != "mail.example.com" || a.Mail.Port != 143 || a.Mail.Security != "starttls" {
		t.Errorf("mail block = %+v", a.Mail)
	}
	if a.Mail.ArchiveFolder != "Archief" {
		t.Errorf("archive_folder = %q", a.Mail.ArchiveFolder)
	}
	if len(a.Mail.ExcludeFolders) != 1 {
		t.Errorf("exclude_folders = %v", a.Mail.ExcludeFolders)
	}
	// A vendorless imap block must not be given a vendor by default, or the
	// preset would fight the explicit host.
	if a.Mail.Vendor != "" {
		t.Errorf("vendor = %q, want none for a self-hosted server", a.Mail.Vendor)
	}
}

func TestValidateIMAPRules(t *testing.T) {
	for _, tc := range []struct {
		name, toml, want string
	}{
		{
			name: "imap needs a vendor or a host",
			toml: "[[accounts]]\nname=\"a\"\nemail=\"a@x.com\"\n[accounts.mail]\nbackend=\"imap\"\n",
			want: "vendor or host",
		},
		{
			name: "host is imap-only",
			toml: "[[accounts]]\nname=\"a\"\nemail=\"a@x.com\"\n[accounts.mail]\nbackend=\"gmail\"\nhost=\"x\"\n",
			want: "only applies to an imap",
		},
		{
			name: "security must be known",
			toml: "[[accounts]]\nname=\"a\"\nemail=\"a@x.com\"\n[accounts.mail]\nbackend=\"imap\"\nhost=\"x\"\nsecurity=\"ssl\"\n",
			want: "security must be",
		},
		{
			name: "icloud mail is imap",
			toml: "[[accounts]]\nname=\"a\"\nemail=\"a@x.com\"\n[accounts.mail]\nbackend=\"jmap\"\nvendor=\"icloud\"\n",
			want: "iCloud mail is IMAP",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(writeTemp(t, "config.toml", tc.toml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			err = c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// push now means "a provider stream", which IMAP has via IDLE.
func TestPushIsAllowedOnIMAP(t *testing.T) {
	c, err := Load(writeTemp(t, "config.toml",
		"[[accounts]]\nname=\"a\"\nemail=\"a@x.com\"\npush=true\n[accounts.mail]\nbackend=\"imap\"\nhost=\"x\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// NewAccount is what `account add icloud` writes, and iCloud now has mail.
func TestNewICloudAccountSyncsMailAndCalendars(t *testing.T) {
	a := NewAccount("apple", "me@icloud.example", model.VendorICloud)
	if !a.SyncsMail() || a.Mail.Backend != model.BackendIMAP {
		t.Errorf("mail = %+v, want an imap block", a.Mail)
	}
	if !a.SyncsCalendar() || a.Calendar.Backend != model.BackendCalDAV {
		t.Errorf("calendar = %+v, want a caldav block", a.Calendar)
	}
}

// A config that goes through Save and Load again must come back the same, or
// `account add` quietly loses whatever it wrote.
func TestIMAPSurvivesASaveLoadRoundTrip(t *testing.T) {
	in := NewAccount("home", "me@example.com", "")
	in.Mail = &MailBackend{
		Backend: model.BackendIMAP, Host: "mail.example.com", Port: 143,
		Security: "starttls", Username: "me", SMTPHost: "smtp.example.com",
		SMTPPort: 587, SMTPSecurity: "starttls", ArchiveFolder: "Archief",
		ExcludeFolders: []string{"Junk"}, IncludeAllMail: true,
	}
	cfg := &Config{General: General{DefaultFormat: DefaultFormat, SecretBackend: BackendFile},
		Accounts: []Account{in}}

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := back.Account("home")
	if !ok {
		t.Fatal("account lost")
	}
	if !sameMail(in.Mail, got.Mail) {
		t.Errorf("mail block changed:\n in: %+v\nout: %+v", in.Mail, got.Mail)
	}
}

func TestLoadAIModels(t *testing.T) {
	p := writeTemp(t, "config.toml", `
[[accounts]]
name  = "work"
email = "w@example.com"
  [accounts.mail]
  backend = "jmap"

[ai]
default = "big"

[[ai.models]]
name  = "small"
model = "qwen3:8b"

[[ai.models]]
name    = "big"
backend = "ollama"
model   = "qwen3:32b"
url     = "http://gpu-box:11434"
timeout = "10m"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(c.AI.Models) != 2 {
		t.Fatalf("got %d models, want 2", len(c.AI.Models))
	}
	small := c.AI.Models[0]
	if small.Backend != AIBackendOllama || small.URL != DefaultOllamaURL || small.Timeout != Duration(DefaultAITimeout) {
		t.Errorf("defaults not filled in: %+v", small)
	}
	m, ok := c.DefaultAIModel()
	if !ok || m.Name != "big" {
		t.Fatalf("DefaultAIModel = %+v, %v; want big", m, ok)
	}
	if m.URL != "http://gpu-box:11434" || m.Timeout != Duration(10*time.Minute) {
		t.Errorf("big = %+v", *m)
	}

	c.AI.Default = ""
	if m, ok := c.DefaultAIModel(); !ok || m.Name != "small" {
		t.Errorf("with no default the first model should be it, got %+v", m)
	}
	c.AI.Models = nil
	if _, ok := c.DefaultAIModel(); ok {
		t.Error("no models configured should report none")
	}
}

func TestValidateAI(t *testing.T) {
	base := func() *Config {
		c := Default()
		c.Accounts = []Account{{Name: "w", Email: "w@example.com", Mail: &MailBackend{Backend: model.BackendJMAP}}}
		c.AI.Models = []AIModel{{Name: "local", Backend: AIBackendOllama, Model: "qwen3:8b", URL: DefaultOllamaURL}}
		return c
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("baseline should validate: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"bad name", func(c *Config) { c.AI.Models[0].Name = "Not Valid" }, "name must be"},
		{"dup name", func(c *Config) { c.AI.Models = append(c.AI.Models, c.AI.Models[0]) }, "duplicate model name"},
		{"bad backend", func(c *Config) { c.AI.Models[0].Backend = "openai" }, `backend "openai"`},
		{"no model", func(c *Config) { c.AI.Models[0].Model = " " }, "model is required"},
		{"bad url", func(c *Config) { c.AI.Models[0].URL = "gpu-box:11434" }, "must be an http(s) URL"},
		{"unknown default", func(c *Config) { c.AI.Default = "cloud" }, `ai.default: "cloud"`},
	}
	for _, tc := range cases {
		c := base()
		tc.mut(c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestSaveKeepsAIModels(t *testing.T) {
	dir := t.TempDir()
	orig := Default()
	orig.Accounts = []Account{{Name: "w", Email: "w@example.com", Mail: &MailBackend{Backend: model.BackendJMAP, Vendor: model.VendorFastmail}}}
	orig.AI.Default = "big"
	orig.AI.Models = []AIModel{
		{Name: "small", Backend: AIBackendOllama, Model: "qwen3:8b", URL: DefaultOllamaURL, Timeout: Duration(DefaultAITimeout)},
		{Name: "big", Backend: AIBackendOllama, Model: "qwen3:32b", URL: "http://gpu-box:11434", Timeout: Duration(10 * time.Minute)},
	}
	p := filepath.Join(dir, "config.toml")
	if err := Save(p, orig); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := Load(p)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("round-tripped config does not validate: %v", err)
	}
	if back.AI.Default != orig.AI.Default || len(back.AI.Models) != len(orig.AI.Models) {
		t.Fatalf("ai table changed: %+v vs %+v", back.AI, orig.AI)
	}
	for i := range orig.AI.Models {
		if orig.AI.Models[i] != back.AI.Models[i] {
			t.Errorf("model %d changed:\n orig %+v\n back %+v", i, orig.AI.Models[i], back.AI.Models[i])
		}
	}

	// A config with no models writes no [ai] table at all.
	orig.AI = AI{}
	if err := Save(p, orig); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "[ai]") {
		t.Error("an empty [ai] table should not be written")
	}
}
