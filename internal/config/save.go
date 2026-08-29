package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/teulaert/emlcalsync/internal/model"
)

// Save writes config.toml atomically. An empty path means DefaultPath().
//
// The output is hand-written rather than reflected so the file stays readable:
// comments explain each key and values that equal the default are emitted only
// when they carry information (account name, provider, email). Unknown keys
// from a previously hand-edited file are not preserved.
func Save(path string, c *Config) error {
	if path == "" {
		path = c.Path
	}
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, render(c), 0o600)
}

func render(c *Config) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# emlcal configuration — %s\n", configPath(c))
	b.WriteString("# Secrets are never stored here; see the secrets/ directory next to this file.\n\n")

	b.WriteString("[general]\n")
	if c.General.Timezone != "" {
		b.WriteString("# IANA timezone used for agenda output and free/busy. Empty = system zone.\n")
		fmt.Fprintf(&b, "timezone       = %s\n", quote(c.General.Timezone))
	} else {
		b.WriteString("# timezone     = \"Europe/Amsterdam\"   # default: system zone\n")
	}
	b.WriteString("# auto = table on a TTY, json when piped.\n")
	fmt.Fprintf(&b, "default_format = %s\n", quote(orDefault(c.General.DefaultFormat, DefaultFormat)))
	if c.General.DefaultAccount != "" {
		b.WriteString("# Account used to compose new mail when --account is omitted.\n")
		b.WriteString("# Replies always go out from the account that received the message.\n")
		fmt.Fprintf(&b, "default_account = %s\n", quote(c.General.DefaultAccount))
	}
	b.WriteString("# Messages larger than this are indexed without their attachments. 0 = unlimited.\n")
	fmt.Fprintf(&b, "raw_max_size   = %s\n", quote(c.General.RawMaxSize.String()))
	b.WriteString("# Where OAuth and API tokens live: file | libsecret.\n")
	fmt.Fprintf(&b, "secret_backend = %s\n", quote(orDefault(c.General.SecretBackend, DefaultSecretBackend)))

	// Directories are only written when they were customised; otherwise the
	// XDG defaults keep working if $HOME or $XDG_* change.
	writeDirIfCustom(&b, "config_dir", c.General.ConfigDir, ConfigDir())
	writeDirIfCustom(&b, "data_dir", c.General.DataDir, DataDir())
	writeDirIfCustom(&b, "state_dir", c.General.StateDir, StateDir())

	for i := range c.Accounts {
		a := &c.Accounts[i]
		b.WriteString("\n[[accounts]]\n")
		fmt.Fprintf(&b, "name     = %s\n", quote(a.Name))
		fmt.Fprintf(&b, "email    = %s\n", quote(a.Email))

		// Every scalar key must be written before the first sub-table: once
		// [accounts.mail] is open, a following bare `poll = …` would belong to
		// that table instead of the account.
		def := defaultsFor(a)
		if a.Poll != def.Poll {
			b.WriteString("# Fallback poll interval when there is no push stream.\n")
			fmt.Fprintf(&b, "poll     = %s\n", quote(a.Poll.String()))
		}
		if a.Push != def.Push {
			fmt.Fprintf(&b, "push     = %s\n", strconv.FormatBool(a.Push))
		}
		if a.IncludeSpamTrash != def.IncludeSpamTrash {
			fmt.Fprintf(&b, "include_spam_trash = %s\n", strconv.FormatBool(a.IncludeSpamTrash))
		}
		if a.RawMaxSize != nil {
			b.WriteString("# Overrides general.raw_max_size for this account.\n")
			fmt.Fprintf(&b, "raw_max_size = %s\n", quote(a.RawMaxSize.String()))
		}
		if !equalStrings(a.Calendars, def.Calendars) {
			b.WriteString("# \"*\" syncs every calendar; otherwise list them by name.\n")
			fmt.Fprintf(&b, "calendars = %s\n", quoteList(a.Calendars))
		}
		if a.Concurrency != def.Concurrency {
			fmt.Fprintf(&b, "concurrency = %d\n", a.Concurrency)
		}

		// A block is written iff the account has one; its absence is what
		// switches the resource off. Validate refuses an account with neither.
		if m := a.Mail; m != nil {
			b.WriteString("\n  [accounts.mail]\n")
			fmt.Fprintf(&b, "  backend = %s\n", quote(string(m.Backend)))
			// IMAP always writes its vendor: it is what selects the host, so
			// dropping it as "the default" would leave nothing to connect to.
			if m.Vendor != "" && (m.Backend == model.BackendIMAP || m.Vendor != defaultVendorFor(m.Backend)) {
				fmt.Fprintf(&b, "  vendor  = %s\n", quote(string(m.Vendor)))
			}
			writeMailStrings(&b, [][2]string{
				{"host", m.Host},
				{"security", m.Security},
				{"username", m.Username},
				{"smtp_host", m.SMTPHost},
				{"smtp_security", m.SMTPSecurity},
				{"smtp_username", m.SMTPUsername},
				{"archive_folder", m.ArchiveFolder},
				{"sent_folder", m.SentFolder},
				{"trash_folder", m.TrashFolder},
				{"drafts_folder", m.DraftsFolder},
			})
			if m.Port != 0 {
				fmt.Fprintf(&b, "  port    = %d\n", m.Port)
			}
			if m.SMTPPort != 0 {
				fmt.Fprintf(&b, "  smtp_port = %d\n", m.SMTPPort)
			}
			if m.IncludeAllMail {
				b.WriteString("  include_all_mail = true\n")
			}
			if len(m.Folders) > 0 {
				fmt.Fprintf(&b, "  folders = %s\n", quoteList(m.Folders))
			}
			if len(m.ExcludeFolders) > 0 {
				fmt.Fprintf(&b, "  exclude_folders = %s\n", quoteList(m.ExcludeFolders))
			}
		} else {
			b.WriteString("\n  # No [accounts.mail]: this account syncs calendars only.\n")
		}
		if c := a.Calendar; c != nil {
			b.WriteString("\n  [accounts.calendar]\n")
			fmt.Fprintf(&b, "  backend = %s\n", quote(string(c.Backend)))
			// CalDAV is shared by several vendors, so its vendor is always
			// written: it is what selects the base URL.
			if c.Vendor != "" && (c.Backend == model.BackendCalDAV || c.Vendor != defaultVendorFor(c.Backend)) {
				fmt.Fprintf(&b, "  vendor  = %s\n", quote(string(c.Vendor)))
			}
			if c.BaseURL != "" {
				fmt.Fprintf(&b, "  base_url = %s\n", quote(c.BaseURL))
			}
			if c.Username != "" {
				b.WriteString("  # Basic-auth user, when it is not the address above.\n")
				fmt.Fprintf(&b, "  username = %s\n", quote(c.Username))
			}
		} else {
			b.WriteString("\n  # No [accounts.calendar]: this account syncs mail only.\n")
		}
	}

	// The [ai] table is written whenever it has anything in it, so an
	// `account add` after it was configured by hand does not lose it.
	if len(c.AI.Models) > 0 {
		b.WriteString("\n[ai]\n")
		b.WriteString("# Language models the TUI can draft with (ctrl+g in the composer).\n")
		if c.AI.Default != "" {
			b.WriteString("# The model used when nothing picks one; otherwise the first below.\n")
			fmt.Fprintf(&b, "default = %s\n", quote(c.AI.Default))
		}
		for i := range c.AI.Models {
			m := &c.AI.Models[i]
			b.WriteString("\n[[ai.models]]\n")
			fmt.Fprintf(&b, "name    = %s\n", quote(m.Name))
			if m.Backend != "" && m.Backend != DefaultAIBackend {
				fmt.Fprintf(&b, "backend = %s\n", quote(m.Backend))
			}
			fmt.Fprintf(&b, "model   = %s\n", quote(m.Model))
			if m.URL != "" && !(m.Backend == AIBackendOllama && m.URL == DefaultOllamaURL) {
				fmt.Fprintf(&b, "url     = %s\n", quote(m.URL))
			}
			if m.Timeout != 0 && m.Timeout != Duration(DefaultAITimeout) {
				fmt.Fprintf(&b, "timeout = %s\n", quote(m.Timeout.String()))
			}
		}
	}
	return b.Bytes()
}

// defaultsFor is the account a [[accounts]] table with these backends would
// produce; Save omits any key still at that value.
func defaultsFor(a *Account) Account {
	fa := fileAccount{}
	if a.Mail != nil {
		s := string(a.Mail.Backend)
		fa.Mail = &fileMailBackend{Backend: &s}
	}
	if a.Calendar != nil {
		s := string(a.Calendar.Backend)
		fa.Calendar = &fileCalendarBackend{Backend: &s}
	}
	d, _ := materialize(fa)
	return d
}

func writeDirIfCustom(b *bytes.Buffer, key, val, def string) {
	if val == "" || val == def {
		return
	}
	fmt.Fprintf(b, "%-14s = %s\n", key, quote(val))
}

func configPath(c *Config) string {
	if c.Path != "" {
		return c.Path
	}
	return DefaultPath()
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeMailStrings emits the non-empty string keys of an [accounts.mail] block.
func writeMailStrings(b *bytes.Buffer, pairs [][2]string) {
	for _, kv := range pairs {
		if strings.TrimSpace(kv[1]) != "" {
			fmt.Fprintf(b, "  %-7s = %s\n", kv[0], quote(kv[1]))
		}
	}
}

func quote(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }

func quoteList(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = quote(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// writeFileAtomic writes via a temp file in the same directory + rename, so a
// crash never leaves a half-written config or token behind.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
