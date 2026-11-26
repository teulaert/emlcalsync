package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// [mail] cli_send is off unless the file says otherwise (EML-5).
func TestCLISendDefaultsOff(t *testing.T) {
	c, err := Load(writeTemp(t, "config.toml", "[general]\ntimezone = \"UTC\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mail.CLISend {
		t.Error("cli_send is on without a [mail] table")
	}
	if Default().Mail.CLISend {
		t.Error("cli_send is on in the defaults")
	}
}

func TestCLISendLoadsAndSurvivesSave(t *testing.T) {
	c, err := Load(writeTemp(t, "config.toml", "[mail]\ncli_send = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Mail.CLISend {
		t.Fatal("cli_send = true did not load")
	}
	// An `account add` rewrites the file; the switch must not be lost.
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	again, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Mail.CLISend {
		t.Error("cli_send = true did not survive a save")
	}
}

// Off is not written: a config that never had the switch does not grow one.
func TestCLISendOffIsNotWritten(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(p, Default()); err != nil {
		t.Fatal(err)
	}
	if b := string(mustRead(t, p)); strings.Contains(b, "[mail]") || strings.Contains(b, "cli_send") {
		t.Errorf("a default config mentions the send switch:\n%s", b)
	}
}

func TestMailTableRejectsUnknownKeys(t *testing.T) {
	_, err := Load(writeTemp(t, "config.toml", "[mail]\nclisend = true\n"))
	if err == nil || !strings.Contains(err.Error(), "mail.clisend") {
		t.Errorf("a misspelt switch was accepted: %v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
