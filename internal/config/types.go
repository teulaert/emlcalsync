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

	"github.com/lennert/emlcal/internal/model"
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

// Account is one [[accounts]] entry with defaults applied.
type Account struct {
	Name     string         `toml:"name"`
	Provider model.Provider `toml:"provider"`
	Email    string         `toml:"email"`

	// Poll is the fallback poll interval: 60s for Gmail, 300s for Fastmail
	// (which normally rides the push stream instead).
	Poll Duration `toml:"poll"`
	// Push enables the provider push stream where it exists (Fastmail).
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

	DefaultPollGmail    = 60 * time.Second
	DefaultPollFastmail = 300 * time.Second
)

// EffectiveRawMaxSize resolves the per-account override against the global
// default.
func (a *Account) EffectiveRawMaxSize(g General) Size {
	if a.RawMaxSize != nil {
		return *a.RawMaxSize
	}
	return g.RawMaxSize
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

// SecretKey is the secrets-store key holding this account's credentials:
// "<name>.<provider>.json", matching the on-disk layout in DESIGN.md §3.
func (a *Account) SecretKey() string {
	return a.Name + "." + string(a.Provider) + ".json"
}
