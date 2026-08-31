package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/teulaert/emlcalsync/internal/model"
)

// fileConfig mirrors config.toml with pointers everywhere a default has to be
// distinguishable from an explicit false/zero.
type fileConfig struct {
	General  fileGeneral   `toml:"general"`
	Accounts []fileAccount `toml:"accounts"`
	AI       *fileAI       `toml:"ai"`
}

// fileAI is the [ai] table.
type fileAI struct {
	Default *string       `toml:"default"`
	Models  []fileAIModel `toml:"models"`
}

type fileAIModel struct {
	Name    *string   `toml:"name"`
	Backend *string   `toml:"backend"`
	Model   *string   `toml:"model"`
	URL     *string   `toml:"url"`
	Timeout *Duration `toml:"timeout"`
}

type fileGeneral struct {
	Timezone       *string `toml:"timezone"`
	DefaultFormat  *string `toml:"default_format"`
	DefaultAccount *string `toml:"default_account"`
	RawMaxSize     *Size   `toml:"raw_max_size"`
	ConfigDir      *string `toml:"config_dir"`
	DataDir        *string `toml:"data_dir"`
	StateDir       *string `toml:"state_dir"`
	SecretBackend  *string `toml:"secret_backend"`
}

type fileAccount struct {
	Name             *string              `toml:"name"`
	Email            *string              `toml:"email"`
	Mail             *fileMailBackend     `toml:"mail"`
	Calendar         *fileCalendarBackend `toml:"calendar"`
	Poll             *Duration            `toml:"poll"`
	Push             *bool                `toml:"push"`
	IncludeSpamTrash *bool                `toml:"include_spam_trash"`
	RawMaxSize       *Size                `toml:"raw_max_size"`
	Calendars        []string             `toml:"calendars"`
	Concurrency      *int                 `toml:"concurrency"`
}

// fileMailBackend is the [accounts.mail] table.
type fileMailBackend struct {
	Backend *string `toml:"backend"`
	Vendor  *string `toml:"vendor"`

	Host     *string `toml:"host"`
	Port     *int    `toml:"port"`
	Security *string `toml:"security"`
	Username *string `toml:"username"`

	SMTPHost     *string `toml:"smtp_host"`
	SMTPPort     *int    `toml:"smtp_port"`
	SMTPSecurity *string `toml:"smtp_security"`
	SMTPUsername *string `toml:"smtp_username"`

	Folders        []string `toml:"folders"`
	ExcludeFolders []string `toml:"exclude_folders"`
	IncludeAllMail *bool    `toml:"include_all_mail"`

	ArchiveFolder *string `toml:"archive_folder"`
	SentFolder    *string `toml:"sent_folder"`
	TrashFolder   *string `toml:"trash_folder"`
	DraftsFolder  *string `toml:"drafts_folder"`
}

// fileCalendarBackend is the [accounts.calendar] table.
type fileCalendarBackend struct {
	Backend  *string `toml:"backend"`
	Vendor   *string `toml:"vendor"`
	BaseURL  *string `toml:"base_url"`
	Username *string `toml:"username"`
}

// Load reads config.toml. An empty path means DefaultPath(). A missing file is
// not an error: it yields the defaults with zero accounts, which is what a
// fresh install looks like before `emlcal account add`.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	c := Default()
	c.Path = path

	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		applyEnv(c)
		return c, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var fc fileConfig
	md, err := toml.Decode(string(b), &fc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, 0, len(un))
		for _, k := range un {
			keys = append(keys, k.String())
		}
		sort.Strings(keys)
		return nil, fmt.Errorf("%s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}

	if err := merge(c, &fc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	applyEnv(c)
	return c, nil
}

// Default returns the configuration used when there is no file at all.
func Default() *Config {
	return &Config{
		General: General{
			DefaultFormat: DefaultFormat,
			SecretBackend: DefaultSecretBackend,
			RawMaxSize:    Unlimited,
			ConfigDir:     ConfigDir(),
			DataDir:       DataDir(),
			StateDir:      StateDir(),
		},
	}
}

func merge(c *Config, fc *fileConfig) error {
	g := &fc.General
	setString(&c.General.Timezone, g.Timezone)
	setString(&c.General.DefaultFormat, g.DefaultFormat)
	setString(&c.General.DefaultAccount, g.DefaultAccount)
	setString(&c.General.SecretBackend, g.SecretBackend)
	if g.RawMaxSize != nil {
		c.General.RawMaxSize = *g.RawMaxSize
	}
	if err := setDir(&c.General.ConfigDir, g.ConfigDir); err != nil {
		return fmt.Errorf("general.config_dir: %w", err)
	}
	if err := setDir(&c.General.DataDir, g.DataDir); err != nil {
		return fmt.Errorf("general.data_dir: %w", err)
	}
	if err := setDir(&c.General.StateDir, g.StateDir); err != nil {
		return fmt.Errorf("general.state_dir: %w", err)
	}

	for i, fa := range fc.Accounts {
		a, err := materialize(fa)
		if err != nil {
			return fmt.Errorf("accounts[%d]: %w", i, err)
		}
		c.Accounts = append(c.Accounts, a)
	}
	if fc.AI != nil {
		setString(&c.AI.Default, fc.AI.Default)
		for _, fm := range fc.AI.Models {
			c.AI.Models = append(c.AI.Models, materializeAIModel(fm))
		}
	}
	return nil
}

// materializeAIModel fills in an [[ai.models]] entry's defaults: the backend
// is Ollama unless said otherwise, and its address is the local one.
func materializeAIModel(fm fileAIModel) AIModel {
	m := AIModel{Backend: DefaultAIBackend, Timeout: Duration(DefaultAITimeout)}
	setString(&m.Name, fm.Name)
	setString(&m.Backend, fm.Backend)
	setString(&m.Model, fm.Model)
	setString(&m.URL, fm.URL)
	if fm.Timeout != nil && *fm.Timeout != 0 {
		m.Timeout = *fm.Timeout
	}
	if m.URL == "" && m.Backend == AIBackendOllama {
		m.URL = DefaultOllamaURL
	}
	return m
}

func materialize(fa fileAccount) (Account, error) {
	a := Account{
		IncludeSpamTrash: true,
		Calendars:        []string{"*"},
		Concurrency:      DefaultConcurrency,
	}
	setString(&a.Name, fa.Name)
	setString(&a.Email, fa.Email)

	if fa.Mail != nil {
		mb := MailBackend{}
		setBackend(&mb.Backend, fa.Mail.Backend)
		setVendor(&mb.Vendor, fa.Mail.Vendor)
		setString(&mb.Host, fa.Mail.Host)
		setString(&mb.Security, fa.Mail.Security)
		setString(&mb.Username, fa.Mail.Username)
		setString(&mb.SMTPHost, fa.Mail.SMTPHost)
		setString(&mb.SMTPSecurity, fa.Mail.SMTPSecurity)
		setString(&mb.SMTPUsername, fa.Mail.SMTPUsername)
		setString(&mb.ArchiveFolder, fa.Mail.ArchiveFolder)
		setString(&mb.SentFolder, fa.Mail.SentFolder)
		setString(&mb.TrashFolder, fa.Mail.TrashFolder)
		setString(&mb.DraftsFolder, fa.Mail.DraftsFolder)
		if fa.Mail.Port != nil {
			mb.Port = *fa.Mail.Port
		}
		if fa.Mail.SMTPPort != nil {
			mb.SMTPPort = *fa.Mail.SMTPPort
		}
		if fa.Mail.IncludeAllMail != nil {
			mb.IncludeAllMail = *fa.Mail.IncludeAllMail
		}
		mb.Folders = fa.Mail.Folders
		mb.ExcludeFolders = fa.Mail.ExcludeFolders
		// IMAP is the one mail backend several vendors share, so an absent
		// vendor is not an error — Validate asks for a host instead.
		if mb.Vendor == "" && mb.Host == "" {
			mb.Vendor = defaultVendorFor(mb.Backend)
		}
		a.Mail = &mb
	}
	if fa.Calendar != nil {
		cb := CalendarBackend{}
		setBackend(&cb.Backend, fa.Calendar.Backend)
		setVendor(&cb.Vendor, fa.Calendar.Vendor)
		setString(&cb.BaseURL, fa.Calendar.BaseURL)
		setString(&cb.Username, fa.Calendar.Username)
		if cb.Vendor == "" && cb.BaseURL == "" {
			cb.Vendor = defaultVendorFor(cb.Backend)
		}
		a.Calendar = &cb
	}

	// Backend-shaped defaults, keyed off the mail half; a calendar-only
	// account polls on the calendar cadence instead.
	switch {
	case a.Mail != nil && a.Mail.Backend == model.BackendJMAP:
		a.Poll, a.Push = Duration(DefaultPollJMAP), true
	case a.Mail != nil && a.Mail.Backend == model.BackendIMAP:
		// IDLE watches one folder, so the poll still matters for the rest —
		// but a delta where nothing moved is one round trip per folder.
		a.Poll, a.Push = Duration(DefaultPollIMAPPush), true
	case a.Mail != nil:
		a.Poll, a.Push = Duration(DefaultPollGmail), false
	default:
		a.Poll, a.Push = Duration(DefaultPollCalDAV), false
	}

	if fa.Poll != nil {
		a.Poll = *fa.Poll
	}
	if fa.Push != nil {
		a.Push = *fa.Push
	}
	if fa.IncludeSpamTrash != nil {
		a.IncludeSpamTrash = *fa.IncludeSpamTrash
	}
	if fa.RawMaxSize != nil {
		v := *fa.RawMaxSize
		a.RawMaxSize = &v
	}
	if fa.Calendars != nil {
		a.Calendars = fa.Calendars
	}
	if fa.Concurrency != nil {
		a.Concurrency = *fa.Concurrency
	}
	return a, nil
}

// defaultVendorFor is the vendor a backend implies when the block omits one.
// CalDAV has none: it is the one backend several vendors share, which is why
// Validate insists on a vendor or an explicit base_url.
func defaultVendorFor(b model.Backend) model.Vendor {
	switch b {
	case model.BackendJMAP:
		return model.VendorFastmail
	case model.BackendGmail, model.BackendGCal:
		return model.VendorGoogle
	}
	return ""
}

func setBackend(dst *model.Backend, src *string) {
	if src != nil {
		*dst = model.Backend(strings.ToLower(strings.TrimSpace(*src)))
	}
}

func setVendor(dst *model.Vendor, src *string) {
	if src != nil {
		*dst = model.Vendor(strings.ToLower(strings.TrimSpace(*src)))
	}
}

func setString(dst *string, src *string) {
	if src != nil {
		*dst = strings.TrimSpace(*src)
	}
}

// setDir accepts an absolute path or one starting with "~".
func setDir(dst *string, src *string) error {
	if src == nil {
		return nil
	}
	p := strings.TrimSpace(*src)
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("must be an absolute path, got %q", p)
	}
	*dst = filepath.Clean(p)
	return nil
}

func applyEnv(c *Config) {
	if v := os.Getenv(EnvDataDir); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			c.General.DataDir = abs
		}
	}
	if v := os.Getenv(EnvFormat); v != "" {
		c.General.DefaultFormat = strings.ToLower(strings.TrimSpace(v))
	}
}

// Account returns the named account.
func (c *Config) Account(name string) (*Account, bool) {
	for i := range c.Accounts {
		if c.Accounts[i].Name == name {
			return &c.Accounts[i], true
		}
	}
	return nil, false
}

// AccountNames returns every configured account name, in file order.
// DefaultAIModel is the model AI actions use: ai.default when set, else the
// first configured. ok is false when there are none, which is the off state.
func (c *Config) DefaultAIModel() (*AIModel, bool) {
	if len(c.AI.Models) == 0 {
		return nil, false
	}
	if c.AI.Default == "" {
		return &c.AI.Models[0], true
	}
	for i := range c.AI.Models {
		if c.AI.Models[i].Name == c.AI.Default {
			return &c.AI.Models[i], true
		}
	}
	return nil, false
}

func (c *Config) AccountNames() []string {
	names := make([]string, len(c.Accounts))
	for i := range c.Accounts {
		names[i] = c.Accounts[i].Name
	}
	return names
}

// Location resolves general.timezone, falling back to the system zone.
func (c *Config) Location() (*time.Location, error) {
	if c.General.Timezone == "" {
		return systemZone(), nil
	}
	return time.LoadLocation(c.General.Timezone)
}

// systemZone resolves the system time zone under its IANA name. time.Local
// alone will not do: unless $TZ is set its name is literally "Local", which
// is not an IANA identifier — events created with it are rejected by Google
// Calendar ("Invalid time zone definition") and are no better on JSCalendar.
// So the name is dug out of the places the platform records it, and
// time.Local is only the last resort.
func systemZone() *time.Location {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" && tz != ":" {
		if loc, err := time.LoadLocation(strings.TrimPrefix(tz, ":")); err == nil {
			return loc
		}
	}
	// Debian and friends write the name out.
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if loc, err := time.LoadLocation(strings.TrimSpace(string(b))); err == nil {
			return loc
		}
	}
	// Nearly everywhere else (macOS included) /etc/localtime is a symlink
	// into the zoneinfo tree and the name is the path under it.
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "/zoneinfo/"); ok {
			if loc, err := time.LoadLocation(name); err == nil {
				return loc
			}
		}
	}
	return time.Local
}

// ---------------------------------------------------------------------------
// Validation

// ErrInvalid is the sentinel wrapped by every Validate failure.
var ErrInvalid = errors.New("invalid config")

var validFormats = map[string]bool{"auto": true, "json": true, "table": true, "plain": true}

// Validate checks everything a command would otherwise discover the hard way.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if !validFormats[c.General.DefaultFormat] {
		add("general.default_format: %q is not one of auto, json, table, plain", c.General.DefaultFormat)
	}
	switch c.General.SecretBackend {
	case BackendFile, BackendLibsecret:
	default:
		add("general.secret_backend: %q is not one of %s, %s", c.General.SecretBackend, BackendFile, BackendLibsecret)
	}
	if c.General.Timezone != "" {
		if _, err := time.LoadLocation(c.General.Timezone); err != nil {
			add("general.timezone: %v", err)
		}
	}
	if c.General.RawMaxSize < 0 {
		add("general.raw_max_size: must not be negative")
	}

	seen := make(map[string]bool, len(c.Accounts))
	for i := range c.Accounts {
		a := &c.Accounts[i]
		label := a.Name
		if label == "" {
			label = fmt.Sprintf("accounts[%d]", i)
		}
		if !model.ValidAccountID(a.Name) {
			add("%s: name must be 1-32 chars of [a-z0-9-]", label)
		} else if seen[a.Name] {
			add("%s: duplicate account name", label)
		} else {
			seen[a.Name] = true
		}
		validateBackends(a, label, add)
		if strings.TrimSpace(a.Email) == "" {
			add("%s: email is required", label)
		} else if !strings.Contains(a.Email, "@") {
			add("%s: email %q does not look like an address", label, a.Email)
		}
		if a.Poll < 0 {
			add("%s: poll must not be negative", label)
		}
		if a.Concurrency < 0 {
			add("%s: concurrency must not be negative", label)
		}
		if a.RawMaxSize != nil && *a.RawMaxSize < 0 {
			add("%s: raw_max_size must not be negative", label)
		}
	}

	if c.General.DefaultAccount != "" && !seen[c.General.DefaultAccount] {
		add("general.default_account: %q is not a configured account", c.General.DefaultAccount)
	}

	validateAI(&c.AI, add)

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(errs, "; "))
}

// validateAI checks the [ai] table. Names follow the account-id rules so a
// model can be named on a command line without quoting.
func validateAI(ai *AI, add func(string, ...any)) {
	seen := make(map[string]bool, len(ai.Models))
	for i := range ai.Models {
		m := &ai.Models[i]
		label := "ai.models[" + strconv.Itoa(i) + "]"
		if m.Name != "" {
			label = "ai.models." + m.Name
		}
		if !model.ValidAccountID(m.Name) {
			add("%s: name must be 1-32 chars of [a-z0-9-]", label)
		} else if seen[m.Name] {
			add("%s: duplicate model name", label)
		} else {
			seen[m.Name] = true
		}
		switch m.Backend {
		case AIBackendOllama:
		default:
			add("%s: backend %q is not one of %s", label, m.Backend, AIBackendOllama)
		}
		if strings.TrimSpace(m.Model) == "" {
			add("%s: model is required (the backend's name for it, e.g. \"qwen3:32b\")", label)
		}
		if m.URL != "" {
			u, err := url.Parse(m.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add("%s: url %q must be an http(s) URL", label, m.URL)
			}
		}
		if m.Timeout < 0 {
			add("%s: timeout must not be negative", label)
		}
	}
	if ai.Default != "" && !seen[ai.Default] {
		add("ai.default: %q is not a configured model", ai.Default)
	}
}

// validateBackends checks one account's two resource blocks.
func validateBackends(a *Account, label string, add func(string, ...any)) {
	if a.Mail == nil && a.Calendar == nil {
		add("%s: no [accounts.mail] and no [accounts.calendar] block; the account would sync nothing", label)
		return
	}

	if m := a.Mail; m != nil {
		switch {
		case m.Backend == "":
			add("%s: [%s.mail] backend is required (%s, %s or %s)", label, label,
				model.BackendJMAP, model.BackendGmail, model.BackendIMAP)
		case !m.Backend.Valid(model.MailBackends):
			if m.Backend.Valid(model.CalendarBackends) {
				add("%s: %q is a calendar backend, not a mail backend", label, m.Backend)
			} else {
				add("%s: unknown mail backend %q", label, m.Backend)
			}
		}
		if m.Vendor != "" && !m.Vendor.Valid() {
			add("%s: unknown vendor %q", label, m.Vendor)
		}
		// IMAP is the one mail backend several vendors share, so without a
		// preset or an explicit host there is no server to talk to.
		if m.Backend == model.BackendIMAP && m.Vendor == "" && strings.TrimSpace(m.Host) == "" {
			add("%s: an imap mail backend needs vendor or host", label)
		}
		if m.Backend != model.BackendIMAP && strings.TrimSpace(m.Host) != "" {
			add("%s: host only applies to an imap mail backend", label)
		}
		for _, sec := range []struct{ key, val string }{
			{"security", m.Security}, {"smtp_security", m.SMTPSecurity},
		} {
			switch strings.TrimSpace(sec.val) {
			case "", "tls", "starttls", "none":
			default:
				add("%s: %s must be tls, starttls or none, not %q", label, sec.key, sec.val)
			}
		}
		if m.Vendor == model.VendorICloud && m.Backend != model.BackendIMAP {
			add("%s: iCloud mail is IMAP; backend %q cannot reach it", label, m.Backend)
		}
	}

	if c := a.Calendar; c != nil {
		switch {
		case c.Backend == "":
			add("%s: [%s.calendar] backend is required (%s, %s or %s)", label, label,
				model.BackendCalDAV, model.BackendGCal, model.BackendJMAP)
		case !c.Backend.Valid(model.CalendarBackends):
			if c.Backend.Valid(model.MailBackends) {
				add("%s: %q is a mail backend, not a calendar backend", label, c.Backend)
			} else {
				add("%s: unknown calendar backend %q", label, c.Backend)
			}
		}
		if c.Vendor != "" && !c.Vendor.Valid() {
			add("%s: unknown vendor %q", label, c.Vendor)
		}
		// CalDAV is the one backend several vendors share, so without a vendor
		// preset or an explicit root there is no URL to talk to.
		if c.Backend == model.BackendCalDAV && c.Vendor == "" && strings.TrimSpace(c.BaseURL) == "" {
			add("%s: a caldav calendar needs vendor or base_url", label)
		}
	}

	// push means a provider stream: JMAP's EventSource, or IMAP IDLE. Anything
	// else claiming it is a config lie.
	if a.Push && (a.Mail == nil ||
		(a.Mail.Backend != model.BackendJMAP && a.Mail.Backend != model.BackendIMAP)) {
		add("%s: push requires a jmap or imap mail backend", label)
	}
}
