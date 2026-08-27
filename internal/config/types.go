// Package config loads and validates ~/.config/emlcal/config.toml, resolves
// the XDG directories emlcal stores data in, and opens the secret backend.
//
// Nothing in here touches the network or the database; it is the first thing
// every command does and the only thing that knows where files live.
package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

// ---------------------------------------------------------------------------
// Size

// Size is a byte count that reads and writes as a human string in TOML:
// "0" (unlimited), "25MB", "1GiB". Decimal suffixes (KB/MB/GB/TB) are powers
// of 1000, binary suffixes (KiB/MiB/GiB/TiB) powers of 1024, matching the way
// the units are printed elsewhere in the tool.
type Size int64

// Unlimited is the zero Size: no cap on how much raw mail is archived.
const Unlimited Size = 0

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
	{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
	{"K", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000}, {"T", 1000 * 1000 * 1000 * 1000},
	{"B", 1},
}

// ParseSize parses a byte size such as "0", "512", "25MB" or "1GiB".
func ParseSize(s string) (Size, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if t == "" {
		return 0, fmt.Errorf("size: empty value")
	}
	mult := int64(1)
	for _, u := range sizeUnits {
		if strings.HasSuffix(t, u.suffix) {
			t = strings.TrimSpace(strings.TrimSuffix(t, u.suffix))
			mult = u.mult
			break
		}
	}
	if t == "" {
		return 0, fmt.Errorf("size %q: missing number", s)
	}
	if strings.ContainsAny(t, ".,") {
		f, err := strconv.ParseFloat(strings.Replace(t, ",", ".", 1), 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("size %q: not a number", s)
		}
		v := f * float64(mult)
		if v > math.MaxInt64 {
			return 0, fmt.Errorf("size %q: too large", s)
		}
		return Size(v), nil
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: not a number", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("size %q: must not be negative", s)
	}
	if mult != 1 && n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q: too large", s)
	}
	return Size(n * mult), nil
}

// Bytes returns the size in bytes. Zero means unlimited.
func (s Size) Bytes() int64 { return int64(s) }

// String renders the size the way config.toml spells it.
func (s Size) String() string {
	n := int64(s)
	if n == 0 {
		return "0"
	}
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if n >= u.mult && n%u.mult == 0 {
			return strconv.FormatInt(n/u.mult, 10) + u.suffix
		}
	}
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"TB", 1000 * 1000 * 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"MB", 1000 * 1000}, {"KB", 1000}} {
		if n >= u.mult && n%u.mult == 0 {
			return strconv.FormatInt(n/u.mult, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// UnmarshalText implements encoding.TextUnmarshaler for TOML decoding.
func (s *Size) UnmarshalText(b []byte) error {
	v, err := ParseSize(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (s Size) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// ---------------------------------------------------------------------------
// Duration

// Duration wraps time.Duration so it can be written as "60s" or "5m" in TOML.
type Duration time.Duration

// ParseDuration parses a Go duration string ("60s", "5m", "1h30m").
func ParseDuration(s string) (Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("duration %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q: must not be negative", s)
	}
	return Duration(d), nil
}

// Duration returns the wrapped time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalText implements encoding.TextUnmarshaler for TOML decoding.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// ---------------------------------------------------------------------------
// Config

// Config is the whole of config.toml plus the resolved directory layout.
type Config struct {
	General  General   `toml:"general"`
	Accounts []Account `toml:"accounts"`

	// Path is the file this config was loaded from (empty when it did not
	// exist and defaults were used).
	Path string `toml:"-"`
}

// General holds the [general] table.
type General struct {
	// Timezone is an IANA name; empty means the system zone.
	Timezone string `toml:"timezone"`
	// DefaultFormat is "auto", "json", "table" or "plain".
	DefaultFormat string `toml:"default_format"`
	// DefaultAccount names the account used to compose new mail when no
	// --account is given: `mail send` and `mail draft` without --reply fall
	// back to it. Replies are unaffected — they always go out from the
	// account that received the message being answered. Empty means there is
	// no default, and those commands require --account when more than one
	// account is configured.
	DefaultAccount string `toml:"default_account"`
	// RawMaxSize caps the size of messages archived in full; 0 = unlimited.
	RawMaxSize Size `toml:"raw_max_size"`

	// Directory layout, always absolute after Load.
	ConfigDir string `toml:"config_dir"`
	DataDir   string `toml:"data_dir"`
	StateDir  string `toml:"state_dir"`

	// SecretBackend is "file" (default) or "libsecret".
	SecretBackend string `toml:"secret_backend"`
}

// MailBackend is the [accounts.mail] table. Its presence is the on switch: an
// account with no mail block (a Workspace calendar, or an iCloud account from
// before emlcal spoke IMAP) simply has none.
type MailBackend struct {
	// Backend is the protocol: "jmap", "gmail" or "imap".
	Backend model.Backend `toml:"backend"`
	// Vendor is who runs it. Empty means the backend's implied default; for
	// IMAP it selects the preset, and may be empty alongside an explicit Host.
	Vendor model.Vendor `toml:"vendor"`

	// --- imap -----------------------------------------------------------

	// Host overrides the vendor preset's IMAP host: self-hosted servers, and
	// tests pointing at a fake.
	Host string `toml:"host"`
	// Port defaults to 993 for implicit TLS, 143 for STARTTLS.
	Port int `toml:"port"`
	// Security is "tls" (default), "starttls" or "none".
	Security string `toml:"security"`
	// Username is the login when it is not Account.Email. An Apple ID is
	// frequently not the iCloud address.
	Username string `toml:"username"`

	// SMTP submission, because IMAP cannot send. Empty fields take the preset.
	SMTPHost     string `toml:"smtp_host"`
	SMTPPort     int    `toml:"smtp_port"`
	SMTPSecurity string `toml:"smtp_security"`
	SMTPUsername string `toml:"smtp_username"`

	// Folders, when set, is the only folders synced. ExcludeFolders is applied
	// after it.
	Folders        []string `toml:"folders"`
	ExcludeFolders []string `toml:"exclude_folders"`
	// IncludeAllMail syncs a \All mailbox. Off by default: it holds a copy of
	// every message, so on IMAP -- where a copy is a separate message -- it
	// files the whole account twice.
	IncludeAllMail bool `toml:"include_all_mail"`

	// Role overrides for a server whose folders cannot be recognised, either
	// because it has no SPECIAL-USE or because the names are unusual.
	ArchiveFolder string `toml:"archive_folder"`
	SentFolder    string `toml:"sent_folder"`
	TrashFolder   string `toml:"trash_folder"`
	DraftsFolder  string `toml:"drafts_folder"`
}

// User returns the IMAP login, defaulting to the account's own address.
func (m *MailBackend) User(email string) string {
	if s := strings.TrimSpace(m.Username); s != "" {
		return s
	}
	return email
}

// SMTPUser returns the submission login, defaulting to the IMAP one.
func (m *MailBackend) SMTPUser(email string) string {
	if s := strings.TrimSpace(m.SMTPUsername); s != "" {
		return s
	}
	return m.User(email)
}

// CalendarBackend is the [accounts.calendar] table.
type CalendarBackend struct {
	// Backend is the protocol: "caldav", "gcal" or "jmap".
	Backend model.Backend `toml:"backend"`
	// Vendor selects the CalDAV preset (base URL, credential help, primary
	// calendar names). Empty is only allowed alongside an explicit BaseURL.
	Vendor model.Vendor `toml:"vendor"`
	// BaseURL overrides the vendor preset's DAV root: self-hosted CalDAV, and
	// tests pointing at an httptest server.
	BaseURL string `toml:"base_url"`
	// Username is the basic-auth user when it is not Account.Email. An Apple ID
	// is frequently not the iCloud address, while the address is still what
	// must match the account's own ATTENDEE line.
	Username string `toml:"username"`
}

// User returns the basic-auth username for this calendar backend, defaulting to
// the account's own address.
func (c *CalendarBackend) User(email string) string {
	if s := strings.TrimSpace(c.Username); s != "" {
		return s
	}
	return email
}

// Account is one [[accounts]] entry with defaults applied.
//
// Mail and Calendar are the account's two resources. Each is served by its own
// backend with its own credential and its own sync state — a Fastmail account
// is JMAP mail plus CalDAV calendars — and a nil block means the account does
// not sync that resource at all.
type Account struct {
	Name  string `toml:"name"`
	Email string `toml:"email"`

	// Mail is the [accounts.mail] block; nil syncs no mail.
	Mail *MailBackend `toml:"mail"`
	// Calendar is the [accounts.calendar] block; nil syncs no calendars.
	Calendar *CalendarBackend `toml:"calendar"`

	// Poll is the fallback poll interval, shaped by the mail backend: 60s for
	// Gmail, 300s for JMAP (which normally rides the push stream instead).
	// On a calendar-only account it is the calendar poll.
	Poll Duration `toml:"poll"`
	// Push enables the provider push stream where it exists (JMAP).
	Push bool `toml:"push"`
	// IncludeSpamTrash indexes Junk and Trash as well.
	IncludeSpamTrash bool `toml:"include_spam_trash"`
	// RawMaxSize overrides General.RawMaxSize when non-nil.
	RawMaxSize *Size `toml:"raw_max_size"`
	// Calendars lists calendar names to sync; ["*"] means all.
	Calendars []string `toml:"calendars"`
	// Concurrency is the number of in-flight provider requests.
	Concurrency int `toml:"concurrency"`
}

// Defaults that apply when a key is absent from config.toml.
const (
	DefaultFormat        = "auto"
	DefaultSecretBackend = "file"
	DefaultConcurrency   = 4

	DefaultPollGmail  = 60 * time.Second
	DefaultPollJMAP   = 300 * time.Second
	DefaultPollCalDAV = 300 * time.Second
	// DefaultPollIMAP is the fallback when IDLE is not carrying the account.
	// With push on, the poll is only a safety net, so it can be slower.
	DefaultPollIMAP     = 120 * time.Second
	DefaultPollIMAPPush = 300 * time.Second
)

// NewAccount returns the account `emlcal account add <vendor>` would write.
//
// It exists because the zero Account syncs nothing: with per-resource backends
// the blocks are pointers, so a hand-built config.Account{Name: …} has neither
// half switched on.
func NewAccount(name, email string, v model.Vendor) Account {
	a := Account{
		Name:             name,
		Email:            email,
		IncludeSpamTrash: true,
		Calendars:        []string{"*"},
		Concurrency:      DefaultConcurrency,
	}
	switch v {
	case model.VendorGoogle:
		a.Mail = &MailBackend{Backend: model.BackendGmail, Vendor: v}
		a.Calendar = &CalendarBackend{Backend: model.BackendGCal, Vendor: v}
		a.Poll = Duration(DefaultPollGmail)
	case model.VendorFastmail:
		a.Mail = &MailBackend{Backend: model.BackendJMAP, Vendor: v}
		a.Calendar = &CalendarBackend{Backend: model.BackendCalDAV, Vendor: v}
		a.Poll = Duration(DefaultPollJMAP)
		a.Push = true
	case model.VendorICloud:
		// Mail over IMAP, calendars over CalDAV — one app-specific password
		// serves both.
		a.Mail = &MailBackend{Backend: model.BackendIMAP, Vendor: v}
		a.Calendar = &CalendarBackend{Backend: model.BackendCalDAV, Vendor: v}
		a.Poll = Duration(DefaultPollIMAPPush)
		a.Push = true
	}
	return a
}

// EffectiveRawMaxSize resolves the per-account override against the global
// default.
func (a *Account) EffectiveRawMaxSize(g General) Size {
	if a.RawMaxSize != nil {
		return *a.RawMaxSize
	}
	return g.RawMaxSize
}

// SyncsMail reports whether the account has a mail backend.
func (a *Account) SyncsMail() bool { return a.Mail != nil }

// SyncsCalendar reports whether the account has a calendar backend.
func (a *Account) SyncsCalendar() bool { return a.Calendar != nil }

// Syncs reports whether the account has this resource enabled. resource is
// "mail" or "calendar"; anything else is false.
func (a *Account) Syncs(resource string) bool {
	switch resource {
	case "mail":
		return a.SyncsMail()
	case "calendar":
		return a.SyncsCalendar()
	}
	return false
}

// Vendor is the account's primary vendor: the mail block's, else the calendar
// block's. It is what the accounts table records, and it is informational — an
// account may mix vendors across its resources.
func (a *Account) Vendor() model.Vendor {
	if a.Mail != nil && a.Mail.Vendor != "" {
		return a.Mail.Vendor
	}
	if a.Calendar != nil {
		return a.Calendar.Vendor
	}
	return ""
}

// SyncsAllCalendars reports whether Calendars is the "*" wildcard.
func (a *Account) SyncsAllCalendars() bool {
	for _, c := range a.Calendars {
		if c == "*" {
			return true
		}
	}
	return false
}

// WantsCalendar reports whether a calendar with this name should be synced.
func (a *Account) WantsCalendar(name string) bool {
	if a.SyncsAllCalendars() {
		return true
	}
	for _, c := range a.Calendars {
		if strings.EqualFold(c, name) {
			return true
		}
	}
	return false
}
