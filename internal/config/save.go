package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lennert/emlcal/internal/model"
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
		fmt.Fprintf(&b, "provider = %s\n", quote(string(a.Provider)))
		fmt.Fprintf(&b, "email    = %s\n", quote(a.Email))

		def := defaultsFor(a.Provider)
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
	}
	return b.Bytes()
}

// defaultsFor is the account a bare [[accounts]] table with this provider
// would produce; Save omits any key still at that value.
func defaultsFor(p model.Provider) Account {
	s := string(p)
	fa := fileAccount{Provider: &s}
	a, _ := materialize(fa)
	return a
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
